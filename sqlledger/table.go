// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger/ledger"
)

// ledgerTable exposes one catalogued table to the engine. Reads go
// through the derived scan keyspace for ordering and through the
// ledger (verified against the root) for row content; writes buffer in
// an editor and land as one atomic ledger commit per statement.
type ledgerTable struct {
	s   *Store
	def *tableDef
}

var _ sql.Table = (*ledgerTable)(nil)
var _ sql.PrimaryKeyTable = (*ledgerTable)(nil)
var _ sql.InsertableTable = (*ledgerTable)(nil)
var _ sql.UpdatableTable = (*ledgerTable)(nil)
var _ sql.DeletableTable = (*ledgerTable)(nil)
var _ sql.TruncateableTable = (*ledgerTable)(nil)
var _ sql.IndexAddressableTable = (*ledgerTable)(nil)
var _ sql.IndexAlterableTable = (*ledgerTable)(nil)

func (t *ledgerTable) Name() string   { return t.def.Name }
func (t *ledgerTable) String() string { return t.def.Name }

func (t *ledgerTable) Collation() sql.CollationID { return sql.Collation_Default }

func (t *ledgerTable) Schema() sql.Schema {
	return t.PrimaryKeySchema().Schema
}

func (t *ledgerTable) PrimaryKeySchema() sql.PrimaryKeySchema {
	schema := make(sql.Schema, len(t.def.Columns))
	pk := make(map[int]bool, len(t.def.PKOrds))
	for _, ord := range t.def.PKOrds {
		pk[ord] = true
	}
	for i := range t.def.Columns {
		c := &t.def.Columns[i]
		typ, err := c.sqlType()
		if err != nil {
			// The catalog only admits supported types; reaching this
			// means a corrupt definition, surfaced on first use.
			panic(fmt.Sprintf("table %s: %v", t.def.Name, err))
		}
		schema[i] = &sql.Column{
			Name:          c.Name,
			Type:          typ,
			Nullable:      c.Nullable && !pk[i],
			AutoIncrement: c.AutoInc,
			Source:        t.def.Name,
			PrimaryKey:    pk[i],
		}
	}
	return sql.PrimaryKeySchema{Schema: schema, PkOrdinals: append([]int(nil), t.def.PKOrds...)}
}

// -- scans -----------------------------------------------------------------

// fullScanPartition scans the whole primary-key keyspace of the table.
type fullScanPartition struct{ prefix []byte }

func (p fullScanPartition) Key() []byte { return p.prefix }

func (t *ledgerTable) Partitions(*sql.Context) (sql.PartitionIter, error) {
	start := scanKey(t.def.ID, nil)
	return sql.PartitionsToPartitionIter(fullScanPartition{prefix: start}), nil
}

func (t *ledgerTable) PartitionRows(ctx *sql.Context, p sql.Partition) (sql.RowIter, error) {
	switch part := p.(type) {
	case fullScanPartition:
		end, ok := incBytes(part.prefix)
		if !ok {
			return nil, fmt.Errorf("table keyspace overflow")
		}
		return newScanIter(t, part.prefix, end, false), nil
	case indexRangePartition:
		if part.reverse {
			return newReverseScanIter(t, part.start, part.end, part.secondary)
		}
		return newScanIter(t, part.start, part.end, part.secondary), nil
	default:
		return nil, fmt.Errorf("unknown partition type %T", p)
	}
}

// newReverseScanIter serves a reversed lookup (ORDER BY … DESC, MAX
// optimisation). The backend iterates forward only, so the range's
// entries are materialised (keys and PK encodings, not rows) and
// walked backwards; rows still resolve one by one through the ledger.
func newReverseScanIter(t *ledgerTable, start, end []byte, secondary bool) (sql.RowIter, error) {
	var pks [][]byte
	next := start
	for {
		t.s.mu.RLock()
		kvs, err := t.s.backend.Scan(next, end, scanChunkSize)
		t.s.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		for _, kv := range kvs {
			if secondary {
				pks = append(pks, kv.Value)
			} else {
				pks = append(pks, kv.Key[9:])
			}
		}
		if len(kvs) < scanChunkSize {
			break
		}
		last := kvs[len(kvs)-1].Key
		next = append(append([]byte(nil), last...), 0x00)
	}
	return &reverseIter{t: t, pks: pks, pos: len(pks) - 1}, nil
}

type reverseIter struct {
	t   *ledgerTable
	pks [][]byte
	pos int
}

func (it *reverseIter) Next(*sql.Context) (sql.Row, error) {
	if it.pos < 0 {
		return nil, io.EOF
	}
	pkEnc := it.pks[it.pos]
	it.pos--
	return it.t.s.readRow(it.t.def, pkEnc)
}

func (it *reverseIter) Close(*sql.Context) error { return nil }

// scanIter walks a keyspace range in chunks. Locks are held only
// inside individual calls, never across engine callbacks. For the
// primary keyspace the PK encoding is the key suffix after the 9-byte
// prefix; for secondary index spaces it is the entry value.
type scanIter struct {
	t         *ledgerTable
	next      []byte // next scan start (inclusive)
	end       []byte // exclusive
	secondary bool
	buf       []ledger.KV
	pos       int
	done      bool
}

func newScanIter(t *ledgerTable, start, end []byte, secondary bool) *scanIter {
	return &scanIter{t: t, next: start, end: end, secondary: secondary}
}

func (it *scanIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		if it.pos < len(it.buf) {
			kv := it.buf[it.pos]
			it.pos++
			var pkEnc []byte
			if it.secondary {
				pkEnc = kv.Value
			} else {
				pkEnc = kv.Key[9:]
			}
			return it.t.s.readRow(it.t.def, pkEnc)
		}
		if it.done {
			return nil, io.EOF
		}
		it.t.s.mu.RLock()
		kvs, err := it.t.s.backend.Scan(it.next, it.end, scanChunkSize)
		it.t.s.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		if len(kvs) < scanChunkSize {
			it.done = true
		}
		if len(kvs) == 0 {
			return nil, io.EOF
		}
		// Resume strictly after the last key of this chunk.
		last := kvs[len(kvs)-1].Key
		it.next = append(append([]byte(nil), last...), 0x00)
		it.buf = kvs
		it.pos = 0
	}
}

func (it *scanIter) Close(*sql.Context) error { return nil }

// readRow fetches and decodes one row by its PK encoding, verified
// against the ledger root.
func (s *Store) readRow(def *tableDef, pkEnc []byte) (sql.Row, error) {
	s.mu.RLock()
	raw, ok, err := s.led.Get(rowKey(def.ID, pkEnc))
	s.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("table %s: scan entry without a ledger row", def.Name)
	}
	if len(raw) < 9 || raw[0] != tagRow || binary.BigEndian.Uint64(raw[1:9]) != def.ID {
		return nil, fmt.Errorf("table %s: row entry is corrupt", def.Name)
	}
	return decodeRow(def.Columns, raw[9:])
}

func (s *Store) rowExists(def *tableDef, pkEnc []byte) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok, err := s.led.Get(rowKey(def.ID, pkEnc))
	return ok, err
}

// -- editors ---------------------------------------------------------------

// pendingRow is the buffered end state of one primary key within a
// statement: old is the committed row being replaced or deleted (nil
// for a pure insert), new is the row to write (nil for a delete).
type pendingRow struct {
	pkEnc []byte
	old   sql.Row
	new   sql.Row
}

type tableEditor struct {
	t       *ledgerTable
	pending map[string]*pendingRow
}

var _ sql.RowInserter = (*tableEditor)(nil)
var _ sql.RowUpdater = (*tableEditor)(nil)
var _ sql.RowDeleter = (*tableEditor)(nil)

func (t *ledgerTable) Inserter(*sql.Context) sql.RowInserter { return t.newEditor() }
func (t *ledgerTable) Updater(*sql.Context) sql.RowUpdater   { return t.newEditor() }
func (t *ledgerTable) Deleter(*sql.Context) sql.RowDeleter   { return t.newEditor() }

func (t *ledgerTable) newEditor() *tableEditor {
	return &tableEditor{t: t, pending: make(map[string]*pendingRow)}
}

func (e *tableEditor) StatementBegin(*sql.Context) {}

func (e *tableEditor) DiscardChanges(_ *sql.Context, _ error) error {
	e.pending = make(map[string]*pendingRow)
	return nil
}

func (e *tableEditor) StatementComplete(*sql.Context) error { return nil }

func (e *tableEditor) Insert(ctx *sql.Context, row sql.Row) error {
	def := e.t.def
	row = append(sql.Row(nil), row...)
	pkEnc, err := encodePK(def, row)
	if err != nil {
		return err
	}
	// An explicitly supplied AUTO_INCREMENT value advances the counter
	// (the engine informs the integrator only through the row itself).
	for i, c := range def.Columns {
		if !c.AutoInc || row[i] == nil {
			continue
		}
		var v uint64
		switch x := row[i].(type) {
		case int64:
			if x > 0 {
				v = uint64(x)
			}
		case uint64:
			v = x
		case int32:
			if x > 0 {
				v = uint64(x)
			}
		case uint32:
			v = uint64(x)
		}
		if v > 0 {
			e.t.s.mu.Lock()
			if v >= def.autoIncNext {
				def.autoIncNext = v + 1
			}
			e.t.s.mu.Unlock()
		}
	}
	key := string(pkEnc)
	if p, ok := e.pending[key]; ok {
		if p.new != nil {
			return sql.ErrPrimaryKeyViolation.New()
		}
		p.new = row // delete then re-insert within the statement
	} else {
		exists, err := e.t.s.rowExists(def, pkEnc)
		if err != nil {
			return err
		}
		if exists {
			return sql.ErrPrimaryKeyViolation.New()
		}
		e.pending[key] = &pendingRow{pkEnc: pkEnc, new: row}
	}
	return e.checkUnique(def, row, pkEnc)
}

func (e *tableEditor) Delete(ctx *sql.Context, row sql.Row) error {
	def := e.t.def
	row = append(sql.Row(nil), row...)
	pkEnc, err := encodePK(def, row)
	if err != nil {
		return err
	}
	key := string(pkEnc)
	if p, ok := e.pending[key]; ok {
		p.new = nil
	} else {
		e.pending[key] = &pendingRow{pkEnc: pkEnc, old: row}
	}
	return nil
}

func (e *tableEditor) Update(ctx *sql.Context, oldRow, newRow sql.Row) error {
	def := e.t.def
	oldPK, err := encodePK(def, oldRow)
	if err != nil {
		return err
	}
	newPK, err := encodePK(def, newRow)
	if err != nil {
		return err
	}
	if string(oldPK) == string(newPK) {
		newRow = append(sql.Row(nil), newRow...)
		key := string(oldPK)
		if p, ok := e.pending[key]; ok {
			p.new = newRow
		} else {
			e.pending[key] = &pendingRow{
				pkEnc: oldPK,
				old:   append(sql.Row(nil), oldRow...),
				new:   newRow,
			}
		}
		return e.checkUnique(def, newRow, oldPK)
	}
	if err := e.Delete(ctx, oldRow); err != nil {
		return err
	}
	return e.Insert(ctx, newRow)
}

// checkUnique enforces unique secondary indexes against committed
// state and the statement buffer. Rows with a NULL in a unique index
// are exempt (MySQL semantics).
func (e *tableEditor) checkUnique(def *tableDef, row sql.Row, pkEnc []byte) error {
	for i := range def.Indexes {
		idx := &def.Indexes[i]
		if !idx.Unique {
			continue
		}
		colsEnc, hasNull, err := encodeIndexCols(def, idx, row)
		if err != nil {
			return err
		}
		if hasNull {
			continue
		}
		// Committed entries under the same column encoding.
		conflict, err := e.t.s.uniqueConflict(idx, colsEnc, pkEnc)
		if err != nil {
			return err
		}
		if !conflict {
			// The conflicting committed row may be deleted or rewritten
			// in this very statement.
			for _, p := range e.pending {
				if string(p.pkEnc) == string(pkEnc) {
					continue
				}
				if p.new == nil {
					continue
				}
				otherEnc, otherNull, err := encodeIndexCols(def, idx, p.new)
				if err != nil {
					return err
				}
				if !otherNull && string(otherEnc) == string(colsEnc) {
					conflict = true
					break
				}
			}
		} else {
			// Committed conflict: unless that committed row is being
			// deleted or moved away in this statement, it stands.
			resolved := false
			for _, p := range e.pending {
				if p.old == nil || string(p.pkEnc) == string(pkEnc) {
					continue
				}
				oldEnc, oldNull, err := encodeIndexCols(def, idx, p.old)
				if err != nil {
					return err
				}
				if oldNull || string(oldEnc) != string(colsEnc) {
					continue
				}
				if p.new == nil {
					resolved = true
					break
				}
				newEnc, newNull, err := encodeIndexCols(def, idx, p.new)
				if err != nil {
					return err
				}
				if newNull || string(newEnc) != string(colsEnc) {
					resolved = true
					break
				}
			}
			conflict = !resolved
		}
		if conflict {
			return sql.NewUniqueKeyErr(idx.Name, false, nil)
		}
	}
	return nil
}

// Close commits the statement: one atomic ledger batch (rows and the
// AUTO_INCREMENT counter) followed by the derived keyspace batch.
func (e *tableEditor) Close(*sql.Context) error {
	if len(e.pending) == 0 {
		return nil
	}
	def := e.t.def
	s := e.t.s
	s.mu.Lock()
	defer s.mu.Unlock()

	var ledOps []ledger.Op
	var scanOps []ledger.BatchOp
	for _, p := range e.pending {
		rk := rowKey(def.ID, p.pkEnc)
		switch {
		case p.new == nil && p.old != nil: // delete
			ledOps = append(ledOps, ledger.Del(rk))
			scanOps = append(scanOps, ledger.BatchOp{Key: scanKey(def.ID, p.pkEnc), Delete: true})
			ops, err := indexOps(def, p.old, p.pkEnc, true)
			if err != nil {
				return err
			}
			scanOps = append(scanOps, ops...)
		case p.new != nil:
			value, err := encodeRow(def.Columns, p.new)
			if err != nil {
				return err
			}
			payload := make([]byte, 0, 9+len(value))
			payload = append(payload, tagRow)
			payload = binary.BigEndian.AppendUint64(payload, def.ID)
			payload = append(payload, value...)
			ledOps = append(ledOps, ledger.Put(rk, payload))
			scanOps = append(scanOps, ledger.BatchOp{Key: scanKey(def.ID, p.pkEnc)})
			if p.old != nil {
				ops, err := indexOps(def, p.old, p.pkEnc, true)
				if err != nil {
					return err
				}
				scanOps = append(scanOps, ops...)
			}
			ops, err := indexOps(def, p.new, p.pkEnc, false)
			if err != nil {
				return err
			}
			scanOps = append(scanOps, ops...)
		}
	}
	if def.hasAutoInc() {
		counter := make([]byte, 0, 9)
		counter = append(counter, tagAutoInc)
		counter = binary.BigEndian.AppendUint64(counter, def.autoIncNext)
		ledOps = append(ledOps, ledger.Put(autoIncKey(def.ID), counter))
	}
	return s.commit(ledOps, scanOps)
}

func (d *tableDef) hasAutoInc() bool {
	for _, c := range d.Columns {
		if c.AutoInc {
			return true
		}
	}
	return false
}

// -- TRUNCATE --------------------------------------------------------------

func (t *ledgerTable) Truncate(ctx *sql.Context) (int, error) {
	s := t.s
	def := t.def
	s.mu.Lock()
	defer s.mu.Unlock()

	count := 0
	for {
		start := scanKey(def.ID, nil)
		end, _ := incBytes(start)
		kvs, err := s.backend.Scan(start, end, scanChunkSize)
		if err != nil {
			return count, err
		}
		if len(kvs) == 0 {
			return count, nil
		}
		var ledOps []ledger.Op
		var scanOps []ledger.BatchOp
		for _, kv := range kvs {
			pkEnc := kv.Key[9:]
			row, err := s.readRowLocked(def, pkEnc)
			if err != nil {
				return count, err
			}
			ledOps = append(ledOps, ledger.Del(rowKey(def.ID, pkEnc)))
			scanOps = append(scanOps, ledger.BatchOp{Key: kv.Key, Delete: true})
			ops, err := indexOps(def, row, pkEnc, true)
			if err != nil {
				return count, err
			}
			scanOps = append(scanOps, ops...)
			count++
		}
		if err := s.commit(ledOps, scanOps); err != nil {
			return count, err
		}
	}
}

// readRowLocked is readRow for callers already holding the lock.
func (s *Store) readRowLocked(def *tableDef, pkEnc []byte) (sql.Row, error) {
	raw, ok, err := s.led.Get(rowKey(def.ID, pkEnc))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("table %s: scan entry without a ledger row", def.Name)
	}
	if len(raw) < 9 || raw[0] != tagRow {
		return nil, fmt.Errorf("table %s: row entry is corrupt", def.Name)
	}
	return decodeRow(def.Columns, raw[9:])
}

// -- AUTO_INCREMENT --------------------------------------------------------

func (t *ledgerTable) PeekNextAutoIncrementValue(*sql.Context) (uint64, error) {
	t.s.mu.RLock()
	defer t.s.mu.RUnlock()
	return t.def.autoIncNext, nil
}

func (t *ledgerTable) GetNextAutoIncrementValue(_ *sql.Context, insertVal interface{}) (uint64, error) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	var given uint64
	switch v := insertVal.(type) {
	case nil:
	case int64:
		if v > 0 {
			given = uint64(v)
		}
	case uint64:
		given = v
	case int:
		if v > 0 {
			given = uint64(v)
		}
	case uint32:
		given = uint64(v)
	case int32:
		if v > 0 {
			given = uint64(v)
		}
	}
	if given == 0 {
		v := t.def.autoIncNext
		t.def.autoIncNext = v + 1
		return v, nil
	}
	if given >= t.def.autoIncNext {
		t.def.autoIncNext = given + 1
	}
	return given, nil
}

type autoIncSetter struct{ t *ledgerTable }

func (t *ledgerTable) AutoIncrementSetter(*sql.Context) sql.AutoIncrementSetter {
	return autoIncSetter{t: t}
}

func (a autoIncSetter) SetAutoIncrementValue(_ *sql.Context, v uint64) error {
	s := a.t.s
	def := a.t.def
	s.mu.Lock()
	defer s.mu.Unlock()
	def.autoIncNext = v
	counter := make([]byte, 0, 9)
	counter = append(counter, tagAutoInc)
	counter = binary.BigEndian.AppendUint64(counter, v)
	return s.commit([]ledger.Op{ledger.Put(autoIncKey(def.ID), counter)}, nil)
}

func (a autoIncSetter) AcquireAutoIncrementLock(*sql.Context) (func(), error) {
	return func() {}, nil
}

func (a autoIncSetter) Close(*sql.Context) error { return nil }
