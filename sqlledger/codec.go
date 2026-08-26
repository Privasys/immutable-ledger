// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"encoding/binary"
	"fmt"
	"math"
	"time"

	"github.com/dolthub/go-mysql-server/sql"
	"github.com/dolthub/go-mysql-server/sql/types"
	query "github.com/dolthub/vitess/go/vt/proto/query"
)

// Value codecs.
//
// Two encodings share the same scalar forms:
//
//   - key encoding: order-preserving bytes used for primary-key and
//     index positions (byte comparison = SQL ordering, binary
//     collation). Strings/bytes are escape-terminated so that
//     multi-column keys keep the prefix property.
//   - row encoding: the stored form of a full row (compact
//     length-framed strings, no escaping).
//
// Scalars: signed integers as big-endian with the sign bit flipped,
// unsigned as plain big-endian, floats via the standard monotone bits
// transform, timestamps as sign-flipped UnixNano.

type colKind uint8

const (
	kindInt colKind = iota + 1
	kindUint
	kindFloat32
	kindFloat64
	kindString
	kindBytes
	kindDatetime
)

// colDef is the persisted definition of one column.
type colDef struct {
	Name      string `json:"name"`
	QType     int32  `json:"qtype"` // query.Type value
	Length    int64  `json:"length,omitempty"`
	Precision int    `json:"precision,omitempty"`
	Nullable  bool   `json:"nullable"`
	AutoInc   bool   `json:"auto_inc,omitempty"`
}

func (c *colDef) kind() (colKind, error) {
	return kindOfQType(query.Type(c.QType))
}

func kindOfQType(qt query.Type) (colKind, error) {
	switch qt {
	case query.Type_INT8, query.Type_INT16, query.Type_INT24, query.Type_INT32, query.Type_INT64:
		return kindInt, nil
	case query.Type_UINT8, query.Type_UINT16, query.Type_UINT24, query.Type_UINT32, query.Type_UINT64:
		return kindUint, nil
	case query.Type_FLOAT32:
		return kindFloat32, nil
	case query.Type_FLOAT64:
		return kindFloat64, nil
	case query.Type_CHAR, query.Type_VARCHAR, query.Type_TEXT:
		return kindString, nil
	case query.Type_BINARY, query.Type_VARBINARY, query.Type_BLOB:
		return kindBytes, nil
	case query.Type_DATETIME, query.Type_TIMESTAMP:
		return kindDatetime, nil
	default:
		return 0, fmt.Errorf("column type %v is not supported", qt)
	}
}

// colDefOf captures a GMS column into a persistable definition.
func colDefOf(col *sql.Column) (colDef, error) {
	qt := col.Type.Type()
	if _, err := kindOfQType(qt); err != nil {
		return colDef{}, fmt.Errorf("column %q: %w", col.Name, err)
	}
	d := colDef{
		Name:     col.Name,
		QType:    int32(qt),
		Nullable: col.Nullable,
		AutoInc:  col.AutoIncrement,
	}
	if st, ok := col.Type.(sql.StringType); ok {
		d.Length = st.MaxCharacterLength()
		if k, _ := kindOfQType(qt); k == kindBytes {
			d.Length = st.MaxByteLength()
		}
	}
	if dt, ok := col.Type.(sql.DatetimeType); ok {
		d.Precision = dt.Precision()
	}
	return d, nil
}

// sqlType reconstructs the GMS type for a stored column definition.
func (c *colDef) sqlType() (sql.Type, error) {
	qt := query.Type(c.QType)
	switch qt {
	case query.Type_INT8:
		return types.Int8, nil
	case query.Type_INT16:
		return types.Int16, nil
	case query.Type_INT24:
		return types.Int24, nil
	case query.Type_INT32:
		return types.Int32, nil
	case query.Type_INT64:
		return types.Int64, nil
	case query.Type_UINT8:
		return types.Uint8, nil
	case query.Type_UINT16:
		return types.Uint16, nil
	case query.Type_UINT24:
		return types.Uint24, nil
	case query.Type_UINT32:
		return types.Uint32, nil
	case query.Type_UINT64:
		return types.Uint64, nil
	case query.Type_FLOAT32:
		return types.Float32, nil
	case query.Type_FLOAT64:
		return types.Float64, nil
	case query.Type_CHAR, query.Type_VARCHAR, query.Type_TEXT:
		return types.CreateStringWithDefaults(qt, c.Length)
	case query.Type_BINARY, query.Type_VARBINARY, query.Type_BLOB:
		return types.CreateBinary(qt, c.Length)
	case query.Type_DATETIME, query.Type_TIMESTAMP:
		return types.CreateDatetimeType(qt, c.Precision)
	default:
		return nil, fmt.Errorf("column type %v is not supported", qt)
	}
}

// -- scalar encodings ------------------------------------------------------

func encInt64(v int64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], uint64(v)^(1<<63))
	return b[:]
}

func decInt64(b []byte) int64 {
	return int64(binary.BigEndian.Uint64(b) ^ (1 << 63))
}

func encUint64(v uint64) []byte {
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], v)
	return b[:]
}

func encFloat64(f float64) []byte {
	b := math.Float64bits(f)
	if b&(1<<63) != 0 {
		b = ^b
	} else {
		b |= 1 << 63
	}
	var out [8]byte
	binary.BigEndian.PutUint64(out[:], b)
	return out[:]
}

func decFloat64(raw []byte) float64 {
	b := binary.BigEndian.Uint64(raw)
	if b&(1<<63) != 0 {
		b &^= 1 << 63
	} else {
		b = ^b
	}
	return math.Float64frombits(b)
}

// coerce turns a GMS value into the codec's canonical Go form for the
// column kind. The engine converts values to the column type before
// they reach the integrator, so this switch is a normalisation, not a
// cast with MySQL semantics.
func coerce(kind colKind, v interface{}) (int64, uint64, float64, string, []byte, time.Time, error) {
	var i int64
	var u uint64
	var f float64
	var s string
	var raw []byte
	var t time.Time
	switch kind {
	case kindInt:
		switch x := v.(type) {
		case int8:
			i = int64(x)
		case int16:
			i = int64(x)
		case int32:
			i = int64(x)
		case int64:
			i = x
		case int:
			i = int64(x)
		case bool:
			if x {
				i = 1
			}
		case uint64:
			i = int64(x)
		default:
			return 0, 0, 0, "", nil, t, fmt.Errorf("unexpected integer value %T", v)
		}
	case kindUint:
		switch x := v.(type) {
		case uint8:
			u = uint64(x)
		case uint16:
			u = uint64(x)
		case uint32:
			u = uint64(x)
		case uint64:
			u = x
		case uint:
			u = uint64(x)
		case int64:
			u = uint64(x)
		default:
			return 0, 0, 0, "", nil, t, fmt.Errorf("unexpected unsigned value %T", v)
		}
	case kindFloat32:
		switch x := v.(type) {
		case float32:
			f = float64(x)
		case float64:
			f = x
		default:
			return 0, 0, 0, "", nil, t, fmt.Errorf("unexpected float value %T", v)
		}
	case kindFloat64:
		switch x := v.(type) {
		case float64:
			f = x
		case float32:
			f = float64(x)
		default:
			return 0, 0, 0, "", nil, t, fmt.Errorf("unexpected float value %T", v)
		}
	case kindString:
		switch x := v.(type) {
		case string:
			s = x
		case []byte:
			s = string(x)
		default:
			return 0, 0, 0, "", nil, t, fmt.Errorf("unexpected string value %T", v)
		}
	case kindBytes:
		switch x := v.(type) {
		case []byte:
			raw = x
		case string:
			raw = []byte(x)
		default:
			return 0, 0, 0, "", nil, t, fmt.Errorf("unexpected bytes value %T", v)
		}
	case kindDatetime:
		switch x := v.(type) {
		case time.Time:
			t = x
		default:
			return 0, 0, 0, "", nil, t, fmt.Errorf("unexpected time value %T", v)
		}
	}
	return i, u, f, s, raw, t, nil
}

// encodeKeyCol appends the order-preserving key form of one column
// value (which must not be nil).
func encodeKeyCol(buf []byte, kind colKind, v interface{}) ([]byte, error) {
	i, u, f, s, raw, t, err := coerce(kind, v)
	if err != nil {
		return nil, err
	}
	switch kind {
	case kindInt:
		return append(buf, encInt64(i)...), nil
	case kindUint:
		return append(buf, encUint64(u)...), nil
	case kindFloat32, kindFloat64:
		return append(buf, encFloat64(f)...), nil
	case kindString:
		return appendEscaped(buf, []byte(s)), nil
	case kindBytes:
		return appendEscaped(buf, raw), nil
	case kindDatetime:
		return append(buf, encInt64(t.UTC().UnixNano())...), nil
	}
	return nil, fmt.Errorf("unhandled kind %d", kind)
}

// appendEscaped writes bytes so that byte comparison of the encoding
// matches byte comparison of the raw values, with a terminator that
// keeps the prefix property across subsequent key columns: 0x00 in the
// data becomes 0x00 0xFF, and 0x00 0x00 terminates.
func appendEscaped(buf, data []byte) []byte {
	for _, b := range data {
		if b == 0x00 {
			buf = append(buf, 0x00, 0xFF)
		} else {
			buf = append(buf, b)
		}
	}
	return append(buf, 0x00, 0x00)
}

// -- row (value) codec -----------------------------------------------------

const (
	rowNull    = 0x00
	rowPresent = 0x01
)

// encodeRow serialises a full row (all columns, schema order).
func encodeRow(cols []colDef, row sql.Row) ([]byte, error) {
	if len(row) != len(cols) {
		return nil, fmt.Errorf("row has %d values, schema has %d columns", len(row), len(cols))
	}
	var buf []byte
	for idx := range cols {
		v := row[idx]
		if v == nil {
			buf = append(buf, rowNull)
			continue
		}
		buf = append(buf, rowPresent)
		kind, err := cols[idx].kind()
		if err != nil {
			return nil, err
		}
		i, u, f, s, raw, t, err := coerce(kind, v)
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", cols[idx].Name, err)
		}
		switch kind {
		case kindInt:
			buf = append(buf, encInt64(i)...)
		case kindUint:
			buf = append(buf, encUint64(u)...)
		case kindFloat32, kindFloat64:
			buf = append(buf, encFloat64(f)...)
		case kindString:
			buf = binary.AppendUvarint(buf, uint64(len(s)))
			buf = append(buf, s...)
		case kindBytes:
			buf = binary.AppendUvarint(buf, uint64(len(raw)))
			buf = append(buf, raw...)
		case kindDatetime:
			buf = append(buf, encInt64(t.UTC().UnixNano())...)
		}
	}
	return buf, nil
}

// decodeRow parses a stored row back into GMS values with the exact Go
// types the column types use.
func decodeRow(cols []colDef, data []byte) (sql.Row, error) {
	row := make(sql.Row, len(cols))
	off := 0
	for idx := range cols {
		if off >= len(data) {
			return nil, fmt.Errorf("row truncated at column %q", cols[idx].Name)
		}
		marker := data[off]
		off++
		if marker == rowNull {
			continue
		}
		if marker != rowPresent {
			return nil, fmt.Errorf("row corrupt at column %q", cols[idx].Name)
		}
		kind, err := cols[idx].kind()
		if err != nil {
			return nil, err
		}
		switch kind {
		case kindInt, kindUint, kindFloat32, kindFloat64, kindDatetime:
			if off+8 > len(data) {
				return nil, fmt.Errorf("row truncated at column %q", cols[idx].Name)
			}
			raw := data[off : off+8]
			off += 8
			switch kind {
			case kindInt:
				row[idx] = narrowInt(query.Type(cols[idx].QType), decInt64(raw))
			case kindUint:
				row[idx] = narrowUint(query.Type(cols[idx].QType), binary.BigEndian.Uint64(raw))
			case kindFloat32:
				row[idx] = float32(decFloat64(raw))
			case kindFloat64:
				row[idx] = decFloat64(raw)
			case kindDatetime:
				row[idx] = time.Unix(0, decInt64(raw)).UTC()
			}
		case kindString, kindBytes:
			n, sz := binary.Uvarint(data[off:])
			if sz <= 0 || off+sz+int(n) > len(data) {
				return nil, fmt.Errorf("row truncated at column %q", cols[idx].Name)
			}
			off += sz
			b := data[off : off+int(n)]
			off += int(n)
			if kind == kindString {
				row[idx] = string(b)
			} else {
				row[idx] = append([]byte(nil), b...)
			}
		}
	}
	if off != len(data) {
		return nil, fmt.Errorf("row has %d trailing bytes", len(data)-off)
	}
	return row, nil
}

func narrowInt(qt query.Type, v int64) interface{} {
	switch qt {
	case query.Type_INT8:
		return int8(v)
	case query.Type_INT16:
		return int16(v)
	case query.Type_INT24, query.Type_INT32:
		return int32(v)
	default:
		return v
	}
}

func narrowUint(qt query.Type, v uint64) interface{} {
	switch qt {
	case query.Type_UINT8:
		return uint8(v)
	case query.Type_UINT16:
		return uint16(v)
	case query.Type_UINT24, query.Type_UINT32:
		return uint32(v)
	default:
		return v
	}
}
