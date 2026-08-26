// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"encoding/binary"
	"fmt"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"

	ledger "github.com/Privasys/immutable-ledger/ledger"
)

// Secondary indexes live in the derived keyspace under the 'x' prefix:
//
//	'x' ‖ index-id8 ‖ cols-enc ‖ pk-enc  →  pk-enc
//
// cols-enc encodes the indexed columns in order-preserving form (a
// NULL marker byte per column: 0x00 NULL, 0x01 present, followed by
// the key encoding when present), so byte order equals index order and
// range predicates become key ranges. The PK encoding rides in both
// the key (distinguishing duplicate column values) and the value
// (direct row addressing). Entries are derived state: rebuilt from the
// ledger whenever the keyspace and the ledger version disagree.
//
// The primary key needs no separate space: the scan keyspace
// ('i' ‖ table-id8 ‖ pk-enc) is its index.

const secondaryPrefix = 'x'

func secondarySpace(idxID uint64) []byte {
	k := make([]byte, 0, 9)
	k = append(k, secondaryPrefix)
	return binary.BigEndian.AppendUint64(k, idxID)
}

// encodeIndexCols builds cols-enc for a row; hasNull reports whether
// any indexed column is NULL (unique enforcement exemption).
func encodeIndexCols(def *tableDef, idx *indexDef, row sql.Row) ([]byte, bool, error) {
	var buf []byte
	hasNull := false
	for _, ord := range idx.ColOrds {
		v := row[ord]
		if v == nil {
			hasNull = true
			buf = append(buf, 0x00)
			continue
		}
		buf = append(buf, 0x01)
		kind, err := def.Columns[ord].kind()
		if err != nil {
			return nil, false, err
		}
		buf, err = encodeKeyCol(buf, kind, v)
		if err != nil {
			return nil, false, fmt.Errorf("index %q column %q: %w", idx.Name, def.Columns[ord].Name, err)
		}
	}
	return buf, hasNull, nil
}

// indexOps returns the derived-keyspace operations that add (or, with
// remove, retract) one row's secondary-index entries.
func indexOps(def *tableDef, row sql.Row, pkEnc []byte, remove bool) ([]ledger.BatchOp, error) {
	var ops []ledger.BatchOp
	for i := range def.Indexes {
		idx := &def.Indexes[i]
		colsEnc, _, err := encodeIndexCols(def, idx, row)
		if err != nil {
			return nil, err
		}
		key := append(secondarySpace(idx.ID), colsEnc...)
		key = append(key, pkEnc...)
		if remove {
			ops = append(ops, ledger.BatchOp{Key: key, Delete: true})
		} else {
			ops = append(ops, ledger.BatchOp{Key: key, Value: pkEnc})
		}
	}
	return ops, nil
}

// indexInsertOps is the rebuild-path form of indexOps.
func indexInsertOps(def *tableDef, row sql.Row, pkEnc []byte) []ledger.BatchOp {
	ops, err := indexOps(def, row, pkEnc, false)
	if err != nil {
		// Rebuild decodes rows the codec itself wrote; an encoding
		// error here means catalog corruption, surfaced by callers.
		return nil
	}
	return ops
}

// uniqueConflict reports whether a committed entry with the same
// column encoding and a different primary key exists. Callers hold no
// lock (it takes the read lock itself).
func (s *Store) uniqueConflict(idx *indexDef, colsEnc, pkEnc []byte) (bool, error) {
	start := append(secondarySpace(idx.ID), colsEnc...)
	end, ok := incBytes(start)
	if !ok {
		return false, fmt.Errorf("index keyspace overflow")
	}
	s.mu.RLock()
	kvs, err := s.backend.Scan(start, end, 2)
	s.mu.RUnlock()
	if err != nil {
		return false, err
	}
	for _, kv := range kvs {
		if string(kv.Value) != string(pkEnc) {
			return true, nil
		}
	}
	return false, nil
}

// incBytes returns the smallest byte string greater than every string
// prefixed by b (false when b is all 0xFF).
func incBytes(b []byte) ([]byte, bool) {
	out := append([]byte(nil), b...)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i] != 0xFF {
			out[i]++
			return out[:i+1], true
		}
	}
	return nil, false
}

// -- sql.Index -------------------------------------------------------------

// ledgerIndex exposes the primary key (idx == nil) or one secondary
// index to the engine.
type ledgerIndex struct {
	dbName string
	def    *tableDef
	idx    *indexDef // nil = PRIMARY
}

var _ sql.Index = (*ledgerIndex)(nil)

func (x *ledgerIndex) ID() string {
	if x.idx == nil {
		return "PRIMARY"
	}
	return x.idx.Name
}

func (x *ledgerIndex) Database() string { return x.dbName }
func (x *ledgerIndex) Table() string    { return x.def.Name }

func (x *ledgerIndex) ords() []int {
	if x.idx == nil {
		return x.def.PKOrds
	}
	return x.idx.ColOrds
}

func (x *ledgerIndex) Expressions() []string {
	ords := x.ords()
	out := make([]string, len(ords))
	for i, ord := range ords {
		out[i] = x.def.Name + "." + x.def.Columns[ord].Name
	}
	return out
}

func (x *ledgerIndex) IsUnique() bool {
	return x.idx == nil || x.idx.Unique
}

func (x *ledgerIndex) IsSpatial() bool  { return false }
func (x *ledgerIndex) IsFullText() bool { return false }
func (x *ledgerIndex) IsVector() bool   { return false }
func (x *ledgerIndex) Comment() string  { return "" }
func (x *ledgerIndex) IndexType() string {
	return "BTREE"
}
func (x *ledgerIndex) IsGenerated() bool       { return false }
func (x *ledgerIndex) PrefixLengths() []uint16 { return nil }

func (x *ledgerIndex) CanSupportOrderBy(sql.Expression) bool { return false }

func (x *ledgerIndex) ColumnExpressionTypes() []sql.ColumnExpressionType {
	ords := x.ords()
	out := make([]sql.ColumnExpressionType, len(ords))
	for i, ord := range ords {
		typ, err := x.def.Columns[ord].sqlType()
		if err != nil {
			typ = nil
		}
		out[i] = sql.ColumnExpressionType{
			Expression: x.def.Name + "." + x.def.Columns[ord].Name,
			Type:       typ,
		}
	}
	return out
}

// CanSupport accepts the range shapes the keyspace can serve as a
// SUPERSET scan: a prefix of exact matches (values or NULL points),
// optionally followed by one bounded column; anything beyond widens to
// the whole subrange. PreciseMatch is false, so the engine re-applies
// its filters over whatever the scan returns.
func (x *ledgerIndex) CanSupport(_ *sql.Context, ranges ...sql.Range) bool {
	for _, r := range ranges {
		mr, ok := r.(sql.MySQLRange)
		if !ok {
			return false
		}
		if _, _, ok := x.rangeBounds(mr); !ok {
			return false
		}
	}
	return true
}

// rangeBounds converts one MySQL range into keyspace scan bounds.
func (x *ledgerIndex) rangeBounds(rng sql.MySQLRange) (start, end []byte, ok bool) {
	var space []byte
	if x.idx == nil {
		space = scanKey(x.def.ID, nil)
	} else {
		space = secondarySpace(x.idx.ID)
	}
	prefix := append([]byte(nil), space...)
	ords := x.ords()

	for i, expr := range rng {
		if i >= len(ords) {
			break
		}
		kind, err := x.def.Columns[ords[i]].kind()
		if err != nil {
			return nil, nil, false
		}
		marker := byte(0x01)
		if x.idx == nil {
			marker = 0 // the PK keyspace has no NULL markers
		}

		lo, hi := expr.LowerBound, expr.UpperBound

		// Exact NULL point: [BelowNull, AboveNull].
		if _, isBN := lo.(sql.BelowNull); isBN {
			if _, isAN := hi.(sql.AboveNull); isAN && x.idx != nil {
				prefix = append(prefix, 0x00)
				continue
			}
		}
		// Exact value: [Below{v}, Above{v}].
		if lb, isB := lo.(sql.Below); isB {
			if ub, isA := hi.(sql.Above); isA {
				lenc, err := encodeCut(kind, lb.Key)
				if err != nil {
					return nil, nil, false
				}
				uenc, err := encodeCut(kind, ub.Key)
				if err != nil {
					return nil, nil, false
				}
				if string(lenc) == string(uenc) {
					if marker != 0 {
						prefix = append(prefix, marker)
					}
					prefix = append(prefix, lenc...)
					continue
				}
			}
		}

		// One ranged column, remainder unbounded.
		start, end, ok = boundedColumn(prefix, marker, kind, lo, hi)
		return start, end, ok
	}

	// Every consumed column was exact: scan the prefix.
	end, ok = incBytes(prefix)
	if !ok {
		return nil, nil, false
	}
	return prefix, end, true
}

// encodeCut key-encodes a cut value.
func encodeCut(kind colKind, v interface{}) ([]byte, error) {
	return encodeKeyCol(nil, kind, v)
}

// boundedColumn builds scan bounds for the first non-exact column.
func boundedColumn(prefix []byte, marker byte, kind colKind, lo, hi sql.MySQLRangeCut) (start, end []byte, ok bool) {
	withMarker := func(enc []byte) []byte {
		out := append([]byte(nil), prefix...)
		if marker != 0 {
			out = append(out, marker)
		}
		return append(out, enc...)
	}

	switch c := lo.(type) {
	case sql.BelowNull:
		start = append([]byte(nil), prefix...)
	case sql.AboveNull:
		if marker == 0 {
			start = append([]byte(nil), prefix...)
		} else {
			start = append(append([]byte(nil), prefix...), marker)
		}
	case sql.Below:
		enc, err := encodeCut(kind, c.Key)
		if err != nil {
			return nil, nil, false
		}
		start = withMarker(enc)
	case sql.Above:
		enc, err := encodeCut(kind, c.Key)
		if err != nil {
			return nil, nil, false
		}
		start, ok = incBytes(withMarker(enc))
		if !ok {
			return nil, nil, false
		}
	default:
		return nil, nil, false
	}

	switch c := hi.(type) {
	case sql.AboveAll:
		end, ok = incBytes(prefix)
		if !ok {
			return nil, nil, false
		}
	case sql.Above:
		enc, err := encodeCut(kind, c.Key)
		if err != nil {
			return nil, nil, false
		}
		end, ok = incBytes(withMarker(enc))
		if !ok {
			return nil, nil, false
		}
	case sql.Below:
		enc, err := encodeCut(kind, c.Key)
		if err != nil {
			return nil, nil, false
		}
		end = withMarker(enc)
	case sql.AboveNull:
		if marker == 0 {
			return nil, nil, false
		}
		end = append(append([]byte(nil), prefix...), marker)
	default:
		return nil, nil, false
	}
	return start, end, true
}

// -- table integration -----------------------------------------------------

// indexRangePartition scans [start, end) of one keyspace, optionally
// in reverse key order.
type indexRangePartition struct {
	start, end []byte
	secondary  bool
	reverse    bool
	seq        int
}

func (p indexRangePartition) Key() []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(p.seq))
}

func (t *ledgerTable) GetIndexes(*sql.Context) ([]sql.Index, error) {
	out := []sql.Index{&ledgerIndex{dbName: t.s.dbName, def: t.def}}
	for i := range t.def.Indexes {
		out = append(out, &ledgerIndex{dbName: t.s.dbName, def: t.def, idx: &t.def.Indexes[i]})
	}
	return out, nil
}

func (t *ledgerTable) PreciseMatch() bool { return false }

func (t *ledgerTable) IndexedAccess(_ *sql.Context, lookup sql.IndexLookup) sql.IndexedTable {
	idx, ok := lookup.Index.(*ledgerIndex)
	if !ok || idx.def.ID != t.def.ID {
		return nil
	}
	return &indexedLedgerTable{ledgerTable: t}
}

type indexedLedgerTable struct {
	*ledgerTable
}

func (t *indexedLedgerTable) LookupPartitions(_ *sql.Context, lookup sql.IndexLookup) (sql.PartitionIter, error) {
	idx, ok := lookup.Index.(*ledgerIndex)
	if !ok {
		return nil, fmt.Errorf("unexpected index type %T", lookup.Index)
	}
	ranges, ok := lookup.Ranges.(sql.MySQLRangeCollection)
	if !ok {
		return nil, fmt.Errorf("unexpected range collection type %T", lookup.Ranges)
	}
	if lookup.IsEmptyRange {
		return sql.PartitionsToPartitionIter(), nil
	}
	var parts []sql.Partition
	for i, rng := range ranges {
		start, end, ok := idx.rangeBounds(rng)
		if !ok {
			return nil, fmt.Errorf("index %s cannot serve the requested range", idx.ID())
		}
		parts = append(parts, indexRangePartition{
			start:     start,
			end:       end,
			secondary: idx.idx != nil,
			reverse:   lookup.IsReverse,
			seq:       i,
		})
	}
	if lookup.IsReverse {
		// Partitions themselves stream in reverse range order.
		for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
			parts[i], parts[j] = parts[j], parts[i]
		}
	}
	return sql.PartitionsToPartitionIter(parts...), nil
}

// -- DDL -------------------------------------------------------------------

func (t *ledgerTable) CreateIndex(ctx *sql.Context, indexDefn sql.IndexDef) error {
	if indexDefn.IsFullText() || indexDefn.IsSpatial() || indexDefn.IsVector() {
		return fmt.Errorf("only BTREE indexes are supported")
	}
	name := indexDefn.Name
	if name == "" {
		cols := make([]string, len(indexDefn.Columns))
		for i, c := range indexDefn.Columns {
			cols[i] = c.Name
		}
		name = strings.Join(cols, "_")
	}
	return t.s.createIndex(t.def, name, indexDefn)
}

func (t *ledgerTable) DropIndex(ctx *sql.Context, indexName string) error {
	return t.s.dropIndex(t.def, indexName)
}

func (t *ledgerTable) RenameIndex(ctx *sql.Context, fromIndexName, toIndexName string) error {
	return t.s.renameIndex(t.def, fromIndexName, toIndexName)
}
