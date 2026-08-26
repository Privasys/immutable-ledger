// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

// Package sqlledger runs MySQL-dialect SQL over an immutable-ledger
// store, using go-mysql-server as the query engine.
//
// # Data placement
//
// The ledger remains the sole authenticated record. All SQL state
// lives in it under reserved logical-key prefixes:
//
//	sql/c/meta            catalog root: table list + next table id
//	sql/c/t/<name>        one table definition (JSON)
//	sql/c/a/<id8>         AUTO_INCREMENT counter for a table
//	sql/r/<id8><pk-enc>   one row (all columns, row codec)
//
// Every SQL-written ledger value starts with a tag byte so a full leaf
// scan can classify entries without knowing their logical keys (the
// index rebuild path).
//
// Because the ledger's tree is ordered by keyed-hash paths (by
// design), ordered iteration for scans comes from a derived keyspace
// written directly to the same backend under the 'i' prefix (distinct
// from every ledger record prefix): one entry per row,
// 'i' ‖ id8 ‖ pk-enc, maintained alongside each commit and rebuilt
// from the ledger whenever it does not match the ledger's version.
// The keyspace is a materialisation: rows are always re-read (and
// verified against the root) from the ledger.
//
// # Concurrency and transactions
//
// The store serialises writers and runs statements in autocommit: each
// DML statement is one atomic ledger commit. Multi-statement
// transactions (BEGIN … COMMIT) are not yet supported.
package sqlledger

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger"
)

// Ledger value tags (first byte of every SQL-written ledger value).
const (
	tagCatalogMeta  = 0x01
	tagCatalogTable = 0x02
	tagRow          = 0x03
	tagAutoInc      = 0x04
)

// Backend keyspace prefixes (outside the ledger's record space).
const (
	scanPrefix    = 'i' // 'i' ‖ id8 ‖ pk-enc → ∅ (row presence, ordered)
	scanMetaKey   = 'j' // 'j' → applied ledger version (u64 BE)
	scanChunkSize = 512
)

// tableDef is the persisted definition of one table.
type tableDef struct {
	ID      uint64     `json:"id"`
	Name    string     `json:"name"`
	Columns []colDef   `json:"columns"`
	PKOrds  []int      `json:"pk_ordinals"`
	Comment string     `json:"comment,omitempty"`
	Indexes []indexDef `json:"indexes,omitempty"`

	// autoIncNext caches the persisted counter (0 = none loaded).
	autoIncNext uint64
}

// indexDef is a persisted secondary index definition.
type indexDef struct {
	ID      uint64 `json:"id"`
	Name    string `json:"name"`
	Unique  bool   `json:"unique"`
	ColOrds []int  `json:"col_ordinals"`
}

type catalogMeta struct {
	NextTableID uint64   `json:"next_table_id"`
	NextIndexID uint64   `json:"next_index_id"`
	Tables      []string `json:"tables"`
}

// Store binds a ledger to the SQL layer: catalog, row storage, and the
// derived scan keyspace.
type Store struct {
	mu      sync.RWMutex
	led     *ledger.Store
	backend ledger.Backend
	dbName  string
	meta    catalogMeta
	tables  map[string]*tableDef // keyed by lower-case name
}

// Open binds the SQL layer to a ledger store. backend must be the same
// backend the ledger runs on (the derived scan keyspace lives next to
// the ledger's records). dbName is the SQL database name this store
// serves. On open, the scan keyspace is reconciled with the ledger:
// if it does not match the ledger's version it is rebuilt from a full
// verified leaf scan.
func Open(led *ledger.Store, backend ledger.Backend, dbName string) (*Store, error) {
	s := &Store{
		led:     led,
		backend: backend,
		dbName:  dbName,
		tables:  make(map[string]*tableDef),
	}
	if err := s.loadCatalog(); err != nil {
		return nil, err
	}
	if err := s.reconcileScanSpace(); err != nil {
		return nil, err
	}
	return s, nil
}

// Ledger exposes the underlying ledger (root inspection, proofs,
// pruning). Callers must not write through it while SQL statements
// run.
func (s *Store) Ledger() *ledger.Store { return s.led }

// -- logical keys ----------------------------------------------------------

func metaKey() []byte { return []byte("sql/c/meta") }

func tableKey(name string) []byte {
	return []byte("sql/c/t/" + strings.ToLower(name))
}

func autoIncKey(id uint64) []byte {
	k := []byte("sql/c/a/")
	return binary.BigEndian.AppendUint64(k, id)
}

func rowKeyPrefix(id uint64) []byte {
	k := []byte("sql/r/")
	return binary.BigEndian.AppendUint64(k, id)
}

func rowKey(id uint64, pkEnc []byte) []byte {
	return append(rowKeyPrefix(id), pkEnc...)
}

func scanKey(id uint64, pkEnc []byte) []byte {
	k := make([]byte, 0, 9+len(pkEnc))
	k = append(k, scanPrefix)
	k = binary.BigEndian.AppendUint64(k, id)
	return append(k, pkEnc...)
}

// -- catalog persistence ---------------------------------------------------

func (s *Store) loadCatalog() error {
	raw, ok, err := s.led.Get(metaKey())
	if err != nil {
		return err
	}
	if !ok {
		s.meta = catalogMeta{NextTableID: 1, NextIndexID: 1}
		return nil
	}
	if len(raw) < 1 || raw[0] != tagCatalogMeta {
		return fmt.Errorf("catalog meta entry is corrupt")
	}
	if err := json.Unmarshal(raw[1:], &s.meta); err != nil {
		return fmt.Errorf("catalog meta: %w", err)
	}
	for _, name := range s.meta.Tables {
		def, err := s.loadTable(name)
		if err != nil {
			return err
		}
		s.tables[strings.ToLower(name)] = def
	}
	return nil
}

func (s *Store) loadTable(name string) (*tableDef, error) {
	raw, ok, err := s.led.Get(tableKey(name))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("catalog names table %q but its definition is missing", name)
	}
	if len(raw) < 1 || raw[0] != tagCatalogTable {
		return nil, fmt.Errorf("table %q definition is corrupt", name)
	}
	def := &tableDef{}
	if err := json.Unmarshal(raw[1:], def); err != nil {
		return nil, fmt.Errorf("table %q definition: %w", name, err)
	}
	// AUTO_INCREMENT counter, if any column uses it.
	for _, c := range def.Columns {
		if c.AutoInc {
			craw, ok, err := s.led.Get(autoIncKey(def.ID))
			if err != nil {
				return nil, err
			}
			if ok && len(craw) == 9 && craw[0] == tagAutoInc {
				def.autoIncNext = binary.BigEndian.Uint64(craw[1:])
			}
			if def.autoIncNext == 0 {
				def.autoIncNext = 1
			}
			break
		}
	}
	return def, nil
}

func marshalTagged(tag byte, v interface{}) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return append([]byte{tag}, raw...), nil
}

// -- scan keyspace ---------------------------------------------------------

func appliedVersion(b ledger.Backend) (uint64, error) {
	raw, ok, err := b.Get([]byte{scanMetaKey})
	if err != nil || !ok {
		return 0, err
	}
	if len(raw) != 8 {
		return 0, nil
	}
	return binary.BigEndian.Uint64(raw), nil
}

// reconcileScanSpace rebuilds the derived keyspace when it does not
// match the ledger version (first open, crash between the ledger
// commit and the keyspace write, restored backup, …). Rebuild is a
// full verified leaf scan: correct and simple; cost is proportional to
// store size.
func (s *Store) reconcileScanSpace() error {
	_, version := s.led.Root()
	applied, err := appliedVersion(s.backend)
	if err != nil {
		return err
	}
	if applied == version {
		return nil
	}

	// Drop the whole keyspace.
	start := []byte{scanPrefix}
	end := []byte{scanPrefix + 1}
	for {
		kvs, err := s.backend.Scan(start, end, scanChunkSize)
		if err != nil {
			return err
		}
		if len(kvs) == 0 {
			break
		}
		ops := make([]ledger.BatchOp, 0, len(kvs))
		for _, kv := range kvs {
			ops = append(ops, ledger.BatchOp{Key: kv.Key, Delete: true})
		}
		if err := s.backend.WriteBatch(ops); err != nil {
			return err
		}
		if len(kvs) < scanChunkSize {
			break
		}
	}

	// Re-derive from the ledger's verified leaves. Row values are
	// self-describing (tagRow ‖ id8 ‖ row bytes), so hashed paths are
	// no obstacle.
	var startAfter *ledger.Hash
	var ops []ledger.BatchOp
	for {
		leaves, done, err := s.led.SnapshotLeaves(version, startAfter, scanChunkSize)
		if err != nil {
			return err
		}
		for i := range leaves {
			v := leaves[i].Value
			if len(v) < 9 || v[0] != tagRow {
				continue
			}
			id := binary.BigEndian.Uint64(v[1:9])
			def := s.tableByID(id)
			if def == nil {
				continue // row of a dropped table awaiting pruning
			}
			row, err := decodeRow(def.Columns, v[9:])
			if err != nil {
				return fmt.Errorf("rebuild: table %s: %w", def.Name, err)
			}
			pkEnc, err := encodePK(def, row)
			if err != nil {
				return err
			}
			ops = append(ops, ledger.BatchOp{Key: scanKey(id, pkEnc)})
			ops = append(ops, indexInsertOps(def, row, pkEnc)...)
			if len(ops) >= scanChunkSize {
				if err := s.backend.WriteBatch(ops); err != nil {
					return err
				}
				ops = ops[:0]
			}
		}
		if done {
			break
		}
		last := leaves[len(leaves)-1].Path
		startAfter = &last
	}
	ops = append(ops, ledger.BatchOp{
		Key:   []byte{scanMetaKey},
		Value: binary.BigEndian.AppendUint64(nil, version),
	})
	return s.backend.WriteBatch(ops)
}

func (s *Store) tableByID(id uint64) *tableDef {
	for _, def := range s.tables {
		if def.ID == id {
			return def
		}
	}
	return nil
}

// commit applies one statement's ledger ops plus the matching derived
// keyspace ops, and advances the applied-version marker. Callers hold
// the write lock.
func (s *Store) commit(ledOps []ledger.Op, scanOps []ledger.BatchOp) error {
	_, _, err := s.led.PutBatch(ledOps)
	if err != nil {
		return err
	}
	_, version := s.led.Root()
	scanOps = append(scanOps, ledger.BatchOp{
		Key:   []byte{scanMetaKey},
		Value: binary.BigEndian.AppendUint64(nil, version),
	})
	return s.backend.WriteBatch(scanOps)
}

// encodePK builds the order-preserving primary-key bytes for a row.
func encodePK(def *tableDef, row sql.Row) ([]byte, error) {
	var buf []byte
	for _, ord := range def.PKOrds {
		v := row[ord]
		if v == nil {
			return nil, fmt.Errorf("primary key column %q is NULL", def.Columns[ord].Name)
		}
		kind, err := def.Columns[ord].kind()
		if err != nil {
			return nil, err
		}
		buf, err = encodeKeyCol(buf, kind, v)
		if err != nil {
			return nil, fmt.Errorf("primary key column %q: %w", def.Columns[ord].Name, err)
		}
	}
	return buf, nil
}
