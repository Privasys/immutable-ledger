// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"fmt"
	"sort"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger/ledger"
)

// Database exposes the store as one SQL database.
type Database struct {
	s *Store
}

var _ sql.Database = (*Database)(nil)
var _ sql.TableCreator = (*Database)(nil)
var _ sql.TableDropper = (*Database)(nil)
var _ sql.TableRenamer = (*Database)(nil)

// NewDatabase wraps a store as a SQL database.
func NewDatabase(s *Store) *Database { return &Database{s: s} }

func (d *Database) Name() string { return d.s.dbName }

func (d *Database) GetTableInsensitive(_ *sql.Context, tblName string) (sql.Table, bool, error) {
	d.s.mu.RLock()
	def, ok := d.s.tables[strings.ToLower(tblName)]
	d.s.mu.RUnlock()
	if !ok {
		return nil, false, nil
	}
	return &ledgerTable{s: d.s, def: def}, true, nil
}

func (d *Database) GetTableNames(*sql.Context) ([]string, error) {
	d.s.mu.RLock()
	defer d.s.mu.RUnlock()
	names := make([]string, 0, len(d.s.tables))
	for _, def := range d.s.tables {
		names = append(names, def.Name)
	}
	sort.Strings(names)
	return names, nil
}

func (d *Database) CreateTable(_ *sql.Context, name string, schema sql.PrimaryKeySchema, collation sql.CollationID, comment string) error {
	if collation != sql.Collation_Unspecified && collation != sql.Collation_Default {
		return fmt.Errorf("only the default (binary-comparable) collation is supported")
	}
	return d.s.createTable(name, schema, comment)
}

func (d *Database) DropTable(_ *sql.Context, name string) error {
	return d.s.dropTable(name)
}

func (d *Database) RenameTable(_ *sql.Context, oldName, newName string) error {
	return d.s.renameTable(oldName, newName)
}

// -- store-side DDL --------------------------------------------------------

func (s *Store) createTable(name string, schema sql.PrimaryKeySchema, comment string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lower := strings.ToLower(name)
	if _, exists := s.tables[lower]; exists {
		return sql.ErrTableAlreadyExists.New(name)
	}
	if len(schema.PkOrdinals) == 0 {
		return fmt.Errorf("table %s: a primary key is required (keyless tables are not supported)", name)
	}
	def := &tableDef{
		ID:      s.meta.NextTableID,
		Name:    name,
		PKOrds:  append([]int(nil), schema.PkOrdinals...),
		Comment: comment,
	}
	for _, col := range schema.Schema {
		cd, err := colDefOf(col)
		if err != nil {
			return fmt.Errorf("table %s: %w", name, err)
		}
		def.Columns = append(def.Columns, cd)
	}
	if def.hasAutoInc() {
		def.autoIncNext = 1
	}

	newMeta := s.meta
	newMeta.NextTableID++
	newMeta.Tables = append(append([]string(nil), s.meta.Tables...), name)

	ledOps, err := catalogOps(newMeta, def)
	if err != nil {
		return err
	}
	if err := s.commit(ledOps, nil); err != nil {
		return err
	}
	s.meta = newMeta
	s.tables[lower] = def
	return nil
}

func catalogOps(meta catalogMeta, defs ...*tableDef) ([]ledger.Op, error) {
	metaRaw, err := marshalTagged(tagCatalogMeta, meta)
	if err != nil {
		return nil, err
	}
	ops := []ledger.Op{ledger.Put(metaKey(), metaRaw)}
	for _, def := range defs {
		defRaw, err := marshalTagged(tagCatalogTable, def)
		if err != nil {
			return nil, err
		}
		ops = append(ops, ledger.Put(tableKey(def.Name), defRaw))
	}
	return ops, nil
}

func (s *Store) dropTable(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	lower := strings.ToLower(name)
	def, ok := s.tables[lower]
	if !ok {
		return sql.ErrTableNotFound.New(name)
	}

	// Delete every row (ledger) and every derived entry, chunked.
	for {
		start := scanKey(def.ID, nil)
		end, _ := incBytes(start)
		kvs, err := s.backend.Scan(start, end, scanChunkSize)
		if err != nil {
			return err
		}
		if len(kvs) == 0 {
			break
		}
		var ledOps []ledger.Op
		var scanOps []ledger.BatchOp
		for _, kv := range kvs {
			pkEnc := kv.Key[9:]
			row, err := s.readRowLocked(def, pkEnc)
			if err != nil {
				return err
			}
			ledOps = append(ledOps, ledger.Del(rowKey(def.ID, pkEnc)))
			scanOps = append(scanOps, ledger.BatchOp{Key: kv.Key, Delete: true})
			ops, err := indexOps(def, row, pkEnc, true)
			if err != nil {
				return err
			}
			scanOps = append(scanOps, ops...)
		}
		if err := s.commit(ledOps, scanOps); err != nil {
			return err
		}
	}

	// Remove the definition, the counter, and the catalog reference.
	newMeta := s.meta
	newMeta.Tables = nil
	for _, n := range s.meta.Tables {
		if !strings.EqualFold(n, name) {
			newMeta.Tables = append(newMeta.Tables, n)
		}
	}
	metaRaw, err := marshalTagged(tagCatalogMeta, newMeta)
	if err != nil {
		return err
	}
	ledOps := []ledger.Op{
		ledger.Put(metaKey(), metaRaw),
		ledger.Del(tableKey(def.Name)),
		ledger.Del(autoIncKey(def.ID)),
	}
	if err := s.commit(ledOps, nil); err != nil {
		return err
	}
	s.meta = newMeta
	delete(s.tables, lower)
	return nil
}

func (s *Store) renameTable(oldName, newName string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	oldLower := strings.ToLower(oldName)
	def, ok := s.tables[oldLower]
	if !ok {
		return sql.ErrTableNotFound.New(oldName)
	}
	if _, exists := s.tables[strings.ToLower(newName)]; exists {
		return sql.ErrTableAlreadyExists.New(newName)
	}

	renamed := *def
	renamed.Name = newName
	newMeta := s.meta
	newMeta.Tables = nil
	for _, n := range s.meta.Tables {
		if strings.EqualFold(n, oldName) {
			newMeta.Tables = append(newMeta.Tables, newName)
		} else {
			newMeta.Tables = append(newMeta.Tables, n)
		}
	}
	ledOps, err := catalogOps(newMeta, &renamed)
	if err != nil {
		return err
	}
	ledOps = append(ledOps, ledger.Del(tableKey(oldName)))
	if err := s.commit(ledOps, nil); err != nil {
		return err
	}
	s.meta = newMeta
	delete(s.tables, oldLower)
	*def = renamed
	s.tables[strings.ToLower(newName)] = def
	return nil
}

// -- index DDL -------------------------------------------------------------

func (s *Store) createIndex(def *tableDef, name string, indexDefn sql.IndexDef) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range def.Indexes {
		if strings.EqualFold(def.Indexes[i].Name, name) {
			return fmt.Errorf("index %s already exists on table %s", name, def.Name)
		}
	}
	colByName := make(map[string]int, len(def.Columns))
	for i, c := range def.Columns {
		colByName[strings.ToLower(c.Name)] = i
	}
	idx := indexDef{
		ID:     s.meta.NextIndexID,
		Name:   name,
		Unique: indexDefn.IsUnique(),
	}
	for _, ic := range indexDefn.Columns {
		ord, ok := colByName[strings.ToLower(ic.Name)]
		if !ok {
			return fmt.Errorf("index %s: unknown column %q", name, ic.Name)
		}
		if ic.Length > 0 {
			return fmt.Errorf("index %s: prefix lengths are not supported", name)
		}
		idx.ColOrds = append(idx.ColOrds, ord)
	}

	// Build entries for existing rows, enforcing uniqueness, in one
	// chunked pass over the table's scan keyspace.
	var entries []ledger.BatchOp
	seen := make(map[string][]byte)
	next := scanKey(def.ID, nil)
	end, _ := incBytes(scanKey(def.ID, nil))
	for {
		kvs, err := s.backend.Scan(next, end, scanChunkSize)
		if err != nil {
			return err
		}
		if len(kvs) == 0 {
			break
		}
		for _, kv := range kvs {
			pkEnc := kv.Key[9:]
			row, err := s.readRowLocked(def, pkEnc)
			if err != nil {
				return err
			}
			colsEnc, hasNull, err := encodeIndexCols(def, &idx, row)
			if err != nil {
				return err
			}
			if idx.Unique && !hasNull {
				if prev, dup := seen[string(colsEnc)]; dup && string(prev) != string(pkEnc) {
					return sql.NewUniqueKeyErr(name, false, nil)
				}
				seen[string(colsEnc)] = pkEnc
			}
			key := append(secondarySpace(idx.ID), colsEnc...)
			key = append(key, pkEnc...)
			entries = append(entries, ledger.BatchOp{Key: key, Value: pkEnc})
		}
		if len(kvs) < scanChunkSize {
			break
		}
		last := kvs[len(kvs)-1].Key
		next = append(append([]byte(nil), last...), 0x00)
	}

	// Persist: catalog first (a ledger commit), then the derived
	// entries (reconciliation rebuilds them if we crash in between).
	updated := *def
	updated.Indexes = append(append([]indexDef(nil), def.Indexes...), idx)
	newMeta := s.meta
	newMeta.NextIndexID++
	ledOps, err := catalogOps(newMeta, &updated)
	if err != nil {
		return err
	}
	if err := s.commit(ledOps, entries); err != nil {
		return err
	}
	s.meta = newMeta
	*def = updated
	return nil
}

func (s *Store) dropIndex(def *tableDef, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pos := -1
	for i := range def.Indexes {
		if strings.EqualFold(def.Indexes[i].Name, name) {
			pos = i
			break
		}
	}
	if pos < 0 {
		return fmt.Errorf("index %s does not exist on table %s", name, def.Name)
	}
	idxID := def.Indexes[pos].ID

	updated := *def
	updated.Indexes = append([]indexDef(nil), def.Indexes...)
	updated.Indexes = append(updated.Indexes[:pos], updated.Indexes[pos+1:]...)
	ledOps, err := catalogOps(s.meta, &updated)
	if err != nil {
		return err
	}

	// Delete the index entries, chunked.
	var scanOps []ledger.BatchOp
	next := secondarySpace(idxID)
	end, _ := incBytes(secondarySpace(idxID))
	for {
		kvs, err := s.backend.Scan(next, end, scanChunkSize)
		if err != nil {
			return err
		}
		for _, kv := range kvs {
			scanOps = append(scanOps, ledger.BatchOp{Key: kv.Key, Delete: true})
		}
		if len(kvs) < scanChunkSize {
			break
		}
		last := kvs[len(kvs)-1].Key
		next = append(append([]byte(nil), last...), 0x00)
	}
	if err := s.commit(ledOps, scanOps); err != nil {
		return err
	}
	*def = updated
	return nil
}

func (s *Store) renameIndex(def *tableDef, from, to string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	updated := *def
	updated.Indexes = append([]indexDef(nil), def.Indexes...)
	found := false
	for i := range updated.Indexes {
		if strings.EqualFold(updated.Indexes[i].Name, from) {
			updated.Indexes[i].Name = to
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("index %s does not exist on table %s", from, def.Name)
	}
	ledOps, err := catalogOps(s.meta, &updated)
	if err != nil {
		return err
	}
	if err := s.commit(ledOps, nil); err != nil {
		return err
	}
	*def = updated
	return nil
}
