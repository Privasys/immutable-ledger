// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger/ledger"
)

// Multi-statement transactions.
//
// A transaction is a per-session buffer over committed state. Each DML
// statement lands in the buffer when its editor closes (statement
// atomicity: a failed statement discards only its own writes); reads
// within the transaction merge the buffer into point lookups and both
// scan keyspaces, so later statements see earlier ones. COMMIT applies
// the whole buffer as one atomic ledger commit — a transaction is
// exactly one ledger version — and ROLLBACK drops it. Nothing is
// persisted before COMMIT, so a crash mid-transaction needs no
// recovery.
//
// Concurrency is optimistic. The buffer records, for every touched
// primary key, the committed row it was based on. If the store
// committed other work after the transaction began, COMMIT re-checks
// each touched key (and unique indexes) against committed state and
// fails with ErrTxnConflict when a touched row changed underneath the
// transaction. Keys the transaction only read are not tracked.
//
// DDL statements commit directly and end the surrounding transaction
// (an implicit commit, as in MySQL).

// ErrTxnConflict reports that a transaction lost a write conflict:
// a row it read and rewrote (or required absent) was changed by a
// commit that landed after the transaction began. The transaction is
// rolled back; retry it.
var ErrTxnConflict = errors.New("transaction write conflict")

// Session is the engine session for one store, carrying transaction
// state. Use one session (and its context) per goroutine.
type Session struct {
	*sql.BaseSession
	store *Store
	txn   *storeTxn
}

var _ sql.Session = (*Session)(nil)
var _ sql.TransactionSession = (*Session)(nil)

// NewSession builds a session over the store.
func NewSession(s *Store) *Session {
	return &Session{BaseSession: sql.NewBaseSession(), store: s}
}

// ledgerTransaction is the engine-facing transaction handle. State
// lives in the session (one active transaction per session).
type ledgerTransaction struct{ readOnly bool }

var _ sql.Transaction = (*ledgerTransaction)(nil)

func (t *ledgerTransaction) String() string   { return "ledger transaction" }
func (t *ledgerTransaction) IsReadOnly() bool { return t.readOnly }

// storeTxn is one transaction's buffered write-set.
type storeTxn struct {
	baseVersion uint64
	readOnly    bool
	tables      map[uint64]*tableTxn // by table id
	savepoints  []savepoint
}

// tableTxn buffers one table's pending rows: pkEnc → end state, where
// old is the committed row the transaction based itself on (nil when
// the key was absent) and new is the row to write (nil for a delete).
type tableTxn struct {
	rows map[string]*pendingRow
}

type savepoint struct {
	name   string
	tables map[uint64]*tableTxn
}

// table returns the buffer for a table, allocating it (write paths).
func (t *storeTxn) table(id uint64) *tableTxn {
	tt, ok := t.tables[id]
	if !ok {
		tt = &tableTxn{rows: make(map[string]*pendingRow)}
		t.tables[id] = tt
	}
	return tt
}

// tableView returns the buffer for a table, or nil (read paths).
func (t *storeTxn) tableView(id uint64) *tableTxn {
	if t == nil {
		return nil
	}
	return t.tables[id]
}

// txnOf extracts the active transaction from a context, if the session
// is ours and one is open.
func txnOf(ctx *sql.Context) *storeTxn {
	if ctx == nil {
		return nil
	}
	if sess, ok := ctx.Session.(*Session); ok {
		return sess.txn
	}
	return nil
}

// snapshotTables deep-copies the buffer (savepoints).
func snapshotTables(tables map[uint64]*tableTxn) map[uint64]*tableTxn {
	out := make(map[uint64]*tableTxn, len(tables))
	for id, tt := range tables {
		rows := make(map[string]*pendingRow, len(tt.rows))
		for k, p := range tt.rows {
			cp := *p
			rows[k] = &cp
		}
		out[id] = &tableTxn{rows: rows}
	}
	return out
}

// -- sql.TransactionSession -------------------------------------------------

func (sess *Session) StartTransaction(_ *sql.Context, ch sql.TransactionCharacteristic) (sql.Transaction, error) {
	readOnly := ch == sql.ReadOnly
	sess.txn = &storeTxn{
		baseVersion: sess.store.version(),
		readOnly:    readOnly,
		tables:      make(map[uint64]*tableTxn),
	}
	return &ledgerTransaction{readOnly: readOnly}, nil
}

func (sess *Session) CommitTransaction(*sql.Context, sql.Transaction) error {
	txn := sess.txn
	if txn == nil {
		return nil
	}
	// A failed commit rolls the transaction back either way (the
	// conflict is against durable state; retrying the same buffer
	// cannot succeed).
	sess.txn = nil
	return sess.store.commitTxn(txn)
}

func (sess *Session) Rollback(*sql.Context, sql.Transaction) error {
	sess.txn = nil
	return nil
}

func (sess *Session) CreateSavepoint(_ *sql.Context, _ sql.Transaction, name string) error {
	txn := sess.txn
	if txn == nil {
		return fmt.Errorf("savepoint %s: no active transaction", name)
	}
	txn.dropSavepoint(name)
	txn.savepoints = append(txn.savepoints, savepoint{
		name:   name,
		tables: snapshotTables(txn.tables),
	})
	return nil
}

func (sess *Session) RollbackToSavepoint(_ *sql.Context, _ sql.Transaction, name string) error {
	txn := sess.txn
	if txn == nil {
		return fmt.Errorf("savepoint %s: no active transaction", name)
	}
	for i := len(txn.savepoints) - 1; i >= 0; i-- {
		if strings.EqualFold(txn.savepoints[i].name, name) {
			txn.tables = snapshotTables(txn.savepoints[i].tables)
			txn.savepoints = txn.savepoints[:i+1]
			return nil
		}
	}
	return sql.ErrSavepointDoesNotExist.New(name)
}

func (sess *Session) ReleaseSavepoint(_ *sql.Context, _ sql.Transaction, name string) error {
	txn := sess.txn
	if txn == nil {
		return fmt.Errorf("savepoint %s: no active transaction", name)
	}
	for i := len(txn.savepoints) - 1; i >= 0; i-- {
		if strings.EqualFold(txn.savepoints[i].name, name) {
			// The named savepoint and every later one are destroyed.
			txn.savepoints = txn.savepoints[:i]
			return nil
		}
	}
	return sql.ErrSavepointDoesNotExist.New(name)
}

func (t *storeTxn) dropSavepoint(name string) {
	for i := range t.savepoints {
		if strings.EqualFold(t.savepoints[i].name, name) {
			t.savepoints = append(t.savepoints[:i], t.savepoints[i+1:]...)
			return
		}
	}
}

// -- commit ----------------------------------------------------------------

// version reads the ledger version under the store lock.
func (s *Store) version() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, v := s.led.Root()
	return v
}

// commitTxn applies a transaction's buffer as one atomic ledger commit
// plus the matching derived-keyspace batch. When the store committed
// other work after the transaction began, every touched key and unique
// index is re-validated against committed state first.
func (s *Store) commitTxn(txn *storeTxn) error {
	if len(txn.tables) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, version := s.led.Root()
	moved := version != txn.baseVersion

	ids := make([]uint64, 0, len(txn.tables))
	for id := range txn.tables {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	var ledOps []ledger.Op
	var scanOps []ledger.BatchOp
	for _, id := range ids {
		tt := txn.tables[id]
		def := s.tableByID(id)
		if def == nil {
			return fmt.Errorf("%w: a table in the write-set was dropped", ErrTxnConflict)
		}
		keys := make([]string, 0, len(tt.rows))
		for k := range tt.rows {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			p := tt.rows[k]
			if p.old == nil && p.new == nil {
				continue // net no-op (inserted then deleted in-transaction)
			}
			if moved {
				if err := s.validatePendingLocked(def, p); err != nil {
					return err
				}
			}
			lo, so, err := buildRowOps(def, p)
			if err != nil {
				return err
			}
			ledOps = append(ledOps, lo...)
			scanOps = append(scanOps, so...)
		}
		if moved {
			if err := s.revalidateUniqueLocked(def, tt); err != nil {
				return err
			}
		}
		if def.hasAutoInc() {
			counter := make([]byte, 0, 9)
			counter = append(counter, tagAutoInc)
			counter = binary.BigEndian.AppendUint64(counter, def.autoIncNext)
			ledOps = append(ledOps, ledger.Put(autoIncKey(def.ID), counter))
		}
	}
	if len(ledOps) == 0 {
		return nil
	}
	return s.commit(ledOps, scanOps)
}

// validatePendingLocked checks that the committed state of one key
// still matches what the transaction based itself on.
func (s *Store) validatePendingLocked(def *tableDef, p *pendingRow) error {
	raw, ok, err := s.led.Get(rowKey(def.ID, p.pkEnc))
	if err != nil {
		return err
	}
	if p.old == nil {
		if ok {
			return fmt.Errorf("%w: table %s: a row this transaction inserts was created concurrently", ErrTxnConflict, def.Name)
		}
		return nil
	}
	if !ok {
		return fmt.Errorf("%w: table %s: a row this transaction rewrites was deleted concurrently", ErrTxnConflict, def.Name)
	}
	oldEnc, err := encodeRow(def.Columns, p.old)
	if err != nil {
		return err
	}
	if len(raw) < 9 || string(raw[9:]) != string(oldEnc) {
		return fmt.Errorf("%w: table %s: a row this transaction rewrites was changed concurrently", ErrTxnConflict, def.Name)
	}
	return nil
}

// revalidateUniqueLocked re-checks unique indexes for the buffered rows
// against committed state (statement-time checks ran before concurrent
// commits landed). A committed conflict stands unless this transaction
// deletes or moves the conflicting row.
func (s *Store) revalidateUniqueLocked(def *tableDef, tt *tableTxn) error {
	for i := range def.Indexes {
		idx := &def.Indexes[i]
		if !idx.Unique {
			continue
		}
		for _, p := range tt.rows {
			if p.new == nil {
				continue
			}
			colsEnc, hasNull, err := encodeIndexCols(def, idx, p.new)
			if err != nil {
				return err
			}
			if hasNull {
				continue
			}
			start := append(secondarySpace(idx.ID), colsEnc...)
			end, ok := incBytes(start)
			if !ok {
				return fmt.Errorf("index keyspace overflow")
			}
			kvs, err := s.backend.Scan(start, end, scanChunkSize)
			if err != nil {
				return err
			}
			for _, kv := range kvs {
				if string(kv.Value) == string(p.pkEnc) {
					continue
				}
				q := tt.rows[string(kv.Value)]
				if q != nil {
					if q.new == nil {
						continue // conflicting row deleted in-transaction
					}
					otherEnc, otherNull, err := encodeIndexCols(def, idx, q.new)
					if err != nil {
						return err
					}
					if otherNull || string(otherEnc) != string(colsEnc) {
						continue // conflicting row moved away in-transaction
					}
				}
				return fmt.Errorf("%w: table %s: unique index %s: a conflicting row was committed concurrently", ErrTxnConflict, def.Name, idx.Name)
			}
		}
	}
	return nil
}

// -- read overlay ----------------------------------------------------------

// scanOverlay is a transaction's contribution to one keyspace scan:
// entries to merge in (sorted) and committed entries to hide.
type scanOverlay struct {
	adds  []ledger.KV
	masks map[string]bool
}

// buildOverlay derives the overlay for a scan of [start, end) over the
// table's primary keyspace (idx == nil) or one secondary index space.
func buildOverlay(def *tableDef, tt *tableTxn, idx *indexDef, start, end []byte) (*scanOverlay, error) {
	if tt == nil || len(tt.rows) == 0 {
		return nil, nil
	}
	ov := &scanOverlay{masks: make(map[string]bool)}
	inRange := func(k []byte) bool {
		return string(k) >= string(start) && string(k) < string(end)
	}
	add := func(k, pkEnc []byte) {
		if inRange(k) {
			delete(ov.masks, string(k))
			ov.adds = append(ov.adds, ledger.KV{Key: k, Value: pkEnc})
		}
	}
	mask := func(k []byte) {
		if inRange(k) {
			ov.masks[string(k)] = true
		}
	}
	for _, p := range tt.rows {
		if idx == nil {
			k := scanKey(def.ID, p.pkEnc)
			if p.new != nil {
				add(k, p.pkEnc)
			} else if p.old != nil {
				mask(k)
			}
			continue
		}
		if p.old != nil {
			colsEnc, _, err := encodeIndexCols(def, idx, p.old)
			if err != nil {
				return nil, err
			}
			k := append(secondarySpace(idx.ID), colsEnc...)
			mask(append(k, p.pkEnc...))
		}
		if p.new != nil {
			colsEnc, _, err := encodeIndexCols(def, idx, p.new)
			if err != nil {
				return nil, err
			}
			k := append(secondarySpace(idx.ID), colsEnc...)
			add(append(k, p.pkEnc...), p.pkEnc)
		}
	}
	if len(ov.adds) == 0 && len(ov.masks) == 0 {
		return nil, nil
	}
	sort.Slice(ov.adds, func(i, j int) bool {
		return string(ov.adds[i].Key) < string(ov.adds[j].Key)
	})
	// An add and a mask on the same key (update leaving indexed columns
	// unchanged) resolve to the add.
	for _, kv := range ov.adds {
		delete(ov.masks, string(kv.Key))
	}
	return ov, nil
}

// mergeOverlay merges backend entries with an overlay into one sorted,
// mask-filtered slice (materialising paths: reverse scans).
func mergeOverlay(entries []ledger.KV, ov *scanOverlay) []ledger.KV {
	if ov == nil {
		return entries
	}
	out := make([]ledger.KV, 0, len(entries)+len(ov.adds))
	i, j := 0, 0
	for i < len(entries) || j < len(ov.adds) {
		switch {
		case i >= len(entries):
			out = append(out, ov.adds[j])
			j++
		case j >= len(ov.adds):
			if !ov.masks[string(entries[i].Key)] {
				out = append(out, entries[i])
			}
			i++
		default:
			c := stringsCompare(ov.adds[j].Key, entries[i].Key)
			if c <= 0 {
				out = append(out, ov.adds[j])
				j++
				if c == 0 {
					i++
				}
			} else {
				if !ov.masks[string(entries[i].Key)] {
					out = append(out, entries[i])
				}
				i++
			}
		}
	}
	return out
}

func stringsCompare(a, b []byte) int {
	switch {
	case string(a) < string(b):
		return -1
	case string(a) > string(b):
		return 1
	default:
		return 0
	}
}

// readRowTxn reads one row by PK encoding through the transaction
// buffer, falling back to the verified committed row.
func (s *Store) readRowTxn(tt *tableTxn, def *tableDef, pkEnc []byte) (sql.Row, error) {
	if tt != nil {
		if p, ok := tt.rows[string(pkEnc)]; ok {
			if p.new == nil {
				return nil, fmt.Errorf("table %s: scan resolved a row deleted in this transaction", def.Name)
			}
			return append(sql.Row(nil), p.new...), nil
		}
	}
	return s.readRow(def, pkEnc)
}
