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

// IsTemporary implements sql.TemporaryTable (always false). The engine
// requires the interface to validate read-only transactions.
func (t *ledgerTable) IsTemporary() bool { return false }

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
	tt := txnOf(ctx).tableView(t.def.ID)
	switch part := p.(type) {
	case fullScanPartition:
		end, ok := incBytes(part.prefix)
		if !ok {
			return nil, fmt.Errorf("table keyspace overflow")
		}
		ov, err := buildOverlay(t.def, tt, nil, part.prefix, end)
		if err != nil {
			return nil, err
		}
		return newScanIter(t, part.prefix, end, false, tt, ov), nil
	case indexRangePartition:
		var idx *indexDef
		if part.secondary {
			if idx = t.def.indexByID(part.idxID); idx == nil {
				return nil, fmt.Errorf("table %s: unknown index id %d", t.def.Name, part.idxID)
			}
		}
		ov, err := buildOverlay(t.def, tt, idx, part.start, part.end)
		if err != nil {
			return nil, err
		}
		if part.reverse {
			return newReverseScanIter(t, part.start, part.end, part.secondary, tt, ov)
		}
		return newScanIter(t, part.start, part.end, part.secondary, tt, ov), nil
	default:
		return nil, fmt.Errorf("unknown partition type %T", p)
	}
}

func (d *tableDef) indexByID(id uint64) *indexDef {
	for i := range d.Indexes {
		if d.Indexes[i].ID == id {
			return &d.Indexes[i]
		}
	}
	return nil
}

// newReverseScanIter serves a reversed lookup (ORDER BY … DESC, MAX
// optimisation). The backend iterates forward only, so the range's
// entries are materialised (keys and PK encodings, not rows), merged
// with the transaction overlay, and walked backwards; rows still
// resolve one by one through the ledger.
func newReverseScanIter(t *ledgerTable, start, end []byte, secondary bool, tt *tableTxn, ov *scanOverlay) (sql.RowIter, error) {
	var entries []ledger.KV
	next := start
	for {
		t.s.mu.RLock()
		kvs, err := t.s.backend.Scan(next, end, scanChunkSize)
		t.s.mu.RUnlock()
		if err != nil {
			return nil, err
		}
		entries = append(entries, kvs...)
		if len(kvs) < scanChunkSize {
			break
		}
		last := kvs[len(kvs)-1].Key
		next = append(append([]byte(nil), last...), 0x00)
	}
	entries = mergeOverlay(entries, ov)
	pks := make([][]byte, 0, len(entries))
	for _, kv := range entries {
		if secondary {
			pks = append(pks, kv.Value)
		} else {
			pks = append(pks, kv.Key[9:])
		}
	}
	return &reverseIter{t: t, tt: tt, pks: pks, pos: len(pks) - 1}, nil
}

type reverseIter struct {
	t   *ledgerTable
	tt  *tableTxn
	pks [][]byte
	pos int
}

func (it *reverseIter) Next(*sql.Context) (sql.Row, error) {
	if it.pos < 0 {
		return nil, io.EOF
	}
	pkEnc := it.pks[it.pos]
	it.pos--
	return it.t.s.readRowTxn(it.tt, it.t.def, pkEnc)
}

func (it *reverseIter) Close(*sql.Context) error { return nil }

// scanIter walks a keyspace range in chunks, merging the transaction
// overlay (if any) into the committed key stream. Locks are held only
// inside individual calls, never across engine callbacks. For the
// primary keyspace the PK encoding is the key suffix after the 9-byte
// prefix; for secondary index spaces it is the entry value.
type scanIter struct {
	t         *ledgerTable
	next      []byte // next scan start (inclusive)
	end       []byte // exclusive
	secondary bool
	tt        *tableTxn
	ov        *scanOverlay
	ovPos     int
	buf       []ledger.KV
	pos       int
	done      bool
}

func newScanIter(t *ledgerTable, start, end []byte, secondary bool, tt *tableTxn, ov *scanOverlay) *scanIter {
	return &scanIter{t: t, next: start, end: end, secondary: secondary, tt: tt, ov: ov}
}

func (it *scanIter) Next(ctx *sql.Context) (sql.Row, error) {
	for {
		if it.pos >= len(it.buf) && !it.done {
			it.t.s.mu.RLock()
			kvs, err := it.t.s.backend.Scan(it.next, it.end, scanChunkSize)
			it.t.s.mu.RUnlock()
			if err != nil {
				return nil, err
			}
			if len(kvs) < scanChunkSize {
				it.done = true
			}
			if len(kvs) > 0 {
				// Resume strictly after the last key of this chunk.
				last := kvs[len(kvs)-1].Key
				it.next = append(append([]byte(nil), last...), 0x00)
			}
			it.buf = kvs
			it.pos = 0
		}
		// Emit the smaller of the committed head and the overlay head;
		// an overlay entry beyond the current chunk waits until the
		// committed stream catches up or ends.
		haveB := it.pos < len(it.buf)
		haveO := it.ov != nil && it.ovPos < len(it.ov.adds)
		var kv ledger.KV
		switch {
		case haveB && haveO:
			c := stringsCompare(it.ov.adds[it.ovPos].Key, it.buf[it.pos].Key)
			if c <= 0 {
				kv = it.ov.adds[it.ovPos]
				it.ovPos++
				if c == 0 {
					it.pos++
				}
			} else {
				kv = it.buf[it.pos]
				it.pos++
				if it.ov.masks[string(kv.Key)] {
					continue
				}
			}
		case haveB:
			kv = it.buf[it.pos]
			it.pos++
			if it.ov != nil && it.ov.masks[string(kv.Key)] {
				continue
			}
		case haveO && it.done:
			kv = it.ov.adds[it.ovPos]
			it.ovPos++
		default:
			if it.done {
				return nil, io.EOF
			}
			continue
		}
		var pkEnc []byte
		if it.secondary {
			pkEnc = kv.Value
		} else {
			pkEnc = kv.Key[9:]
		}
		return it.t.s.readRowTxn(it.tt, it.t.def, pkEnc)
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
	txn     *storeTxn // nil in direct (non-session-transaction) mode
	pending map[string]*pendingRow
}

var _ sql.RowInserter = (*tableEditor)(nil)
var _ sql.RowUpdater = (*tableEditor)(nil)
var _ sql.RowDeleter = (*tableEditor)(nil)

func (t *ledgerTable) Inserter(ctx *sql.Context) sql.RowInserter { return t.newEditor(ctx) }
func (t *ledgerTable) Updater(ctx *sql.Context) sql.RowUpdater   { return t.newEditor(ctx) }
func (t *ledgerTable) Deleter(ctx *sql.Context) sql.RowDeleter   { return t.newEditor(ctx) }

func (t *ledgerTable) newEditor(ctx *sql.Context) *tableEditor {
	return &tableEditor{t: t, txn: txnOf(ctx), pending: make(map[string]*pendingRow)}
}

func (e *tableEditor) writable() error {
	if e.txn != nil && e.txn.readOnly {
		return fmt.Errorf("cannot write in a read-only transaction")
	}
	return nil
}

// txnRow looks a key up in the transaction buffer (not the statement
// buffer).
func (e *tableEditor) txnRow(key string) *pendingRow {
	if e.txn == nil {
		return nil
	}
	tt := e.txn.tableView(e.t.def.ID)
	if tt == nil {
		return nil
	}
	return tt.rows[key]
}

// effectivePending is the union of the statement buffer over the
// transaction buffer, as it will exist after this statement folds: a
// statement row shadowing a transaction row keeps the transaction's
// committed-state old.
func (e *tableEditor) effectivePending() []*pendingRow {
	var tt *tableTxn
	if e.txn != nil {
		tt = e.txn.tableView(e.t.def.ID)
	}
	out := make([]*pendingRow, 0, len(e.pending))
	for k, p := range e.pending {
		if tt != nil {
			if q, ok := tt.rows[k]; ok {
				out = append(out, &pendingRow{pkEnc: p.pkEnc, old: q.old, new: p.new})
				continue
			}
		}
		out = append(out, p)
	}
	if tt != nil {
		for k, p := range tt.rows {
			if _, ok := e.pending[k]; ok {
				continue
			}
			out = append(out, p)
		}
	}
	return out
}

func (e *tableEditor) StatementBegin(*sql.Context) {}

func (e *tableEditor) DiscardChanges(_ *sql.Context, _ error) error {
	e.pending = make(map[string]*pendingRow)
	return nil
}

func (e *tableEditor) StatementComplete(*sql.Context) error { return nil }

func (e *tableEditor) Insert(ctx *sql.Context, row sql.Row) error {
	if err := e.writable(); err != nil {
		return err
	}
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
		var exists bool
		if tp := e.txnRow(key); tp != nil {
			exists = tp.new != nil
		} else {
			var err error
			exists, err = e.t.s.rowExists(def, pkEnc)
			if err != nil {
				return err
			}
		}
		if exists {
			return sql.ErrPrimaryKeyViolation.New()
		}
		e.pending[key] = &pendingRow{pkEnc: pkEnc, new: row}
	}
	return e.checkUnique(def, row, pkEnc)
}

func (e *tableEditor) Delete(ctx *sql.Context, row sql.Row) error {
	if err := e.writable(); err != nil {
		return err
	}
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
	if err := e.writable(); err != nil {
		return err
	}
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
// state and the pending buffers (statement and transaction). Rows with
// a NULL in a unique index are exempt (MySQL semantics).
func (e *tableEditor) checkUnique(def *tableDef, row sql.Row, pkEnc []byte) error {
	pendings := e.effectivePending()
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
			// The conflicting row may be pending in this statement or
			// earlier in the transaction.
			for _, p := range pendings {
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
			// deleted or moved away by pending work, it stands.
			resolved := false
			for _, p := range pendings {
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

// Close ends the statement. In a session transaction the statement
// buffer folds into the transaction buffer (COMMIT does the store
// work); otherwise it commits directly: one atomic ledger batch (rows
// and the AUTO_INCREMENT counter) followed by the derived keyspace
// batch.
func (e *tableEditor) Close(*sql.Context) error {
	if len(e.pending) == 0 {
		return nil
	}
	if e.txn != nil {
		tt := e.txn.table(e.t.def.ID)
		for k, p := range e.pending {
			if q, ok := tt.rows[k]; ok {
				q.new = p.new
			} else {
				tt.rows[k] = p
			}
		}
		e.pending = make(map[string]*pendingRow)
		return nil
	}

	def := e.t.def
	s := e.t.s
	s.mu.Lock()
	defer s.mu.Unlock()

	var ledOps []ledger.Op
	var scanOps []ledger.BatchOp
	for _, p := range e.pending {
		lo, so, err := buildRowOps(def, p)
		if err != nil {
			return err
		}
		ledOps = append(ledOps, lo...)
		scanOps = append(scanOps, so...)
	}
	if def.hasAutoInc() {
		counter := make([]byte, 0, 9)
		counter = append(counter, tagAutoInc)
		counter = binary.BigEndian.AppendUint64(counter, def.autoIncNext)
		ledOps = append(ledOps, ledger.Put(autoIncKey(def.ID), counter))
	}
	return s.commit(ledOps, scanOps)
}

// buildRowOps turns one pending row into its ledger and derived
// keyspace operations.
func buildRowOps(def *tableDef, p *pendingRow) ([]ledger.Op, []ledger.BatchOp, error) {
	var ledOps []ledger.Op
	var scanOps []ledger.BatchOp
	rk := rowKey(def.ID, p.pkEnc)
	switch {
	case p.new == nil && p.old != nil: // delete
		ledOps = append(ledOps, ledger.Del(rk))
		scanOps = append(scanOps, ledger.BatchOp{Key: scanKey(def.ID, p.pkEnc), Delete: true})
		ops, err := indexOps(def, p.old, p.pkEnc, true)
		if err != nil {
			return nil, nil, err
		}
		scanOps = append(scanOps, ops...)
	case p.new != nil:
		value, err := encodeRow(def.Columns, p.new)
		if err != nil {
			return nil, nil, err
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
				return nil, nil, err
			}
			scanOps = append(scanOps, ops...)
		}
		ops, err := indexOps(def, p.new, p.pkEnc, false)
		if err != nil {
			return nil, nil, err
		}
		scanOps = append(scanOps, ops...)
	}
	return ledOps, scanOps, nil
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
	// TRUNCATE is DDL: it commits directly, so buffered transaction
	// rows for this table are discarded rather than replayed on top.
	if txn := txnOf(ctx); txn != nil {
		delete(txn.tables, t.def.ID)
	}
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
