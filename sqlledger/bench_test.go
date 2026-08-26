// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package sqlledger

import (
	"context"
	"fmt"
	"testing"

	"github.com/dolthub/go-mysql-server/sql"

	pebbleback "github.com/Privasys/immutable-ledger/backend/pebble"
	ledger "github.com/Privasys/immutable-ledger/ledger"
)

// SQL benchmarks over the full disk-backed stack (engine + ledger +
// Pebble, synced commits).

type benchEnv struct {
	eng *Engine
	ctx *sql.Context
}

func newBenchEnv(b *testing.B, rows int) *benchEnv {
	b.Helper()
	be, err := pebbleback.Open(b.TempDir(), nil)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = be.Close() })
	led, err := ledger.Create(be, testCK)
	if err != nil {
		b.Fatal(err)
	}
	store, err := Open(led, be, "bench")
	if err != nil {
		b.Fatal(err)
	}
	eng := NewEngine(store)
	ctx := eng.NewContext(context.Background())
	mustExec := func(q string) {
		if _, err := eng.Exec(ctx, q); err != nil {
			b.Fatalf("%s: %v", q, err)
		}
	}
	mustExec(`CREATE TABLE bench (
		id BIGINT PRIMARY KEY,
		name VARCHAR(64) NOT NULL,
		amount DOUBLE NOT NULL,
		grp BIGINT NOT NULL
	)`)
	mustExec(`CREATE INDEX idx_grp ON bench (grp)`)
	for i := 0; i < rows; i += 500 {
		q := "INSERT INTO bench VALUES "
		for j := i; j < i+500 && j < rows; j++ {
			if j > i {
				q += ","
			}
			q += fmt.Sprintf("(%d,'name-%06d',%d.5,%d)", j, j, j, j%100)
		}
		mustExec(q)
	}
	return &benchEnv{eng: eng, ctx: ctx}
}

func BenchmarkSQLInsertSingle(b *testing.B) {
	e := newBenchEnv(b, 10_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := fmt.Sprintf(`INSERT INTO bench VALUES (%d,'x',1.0,7)`, 1_000_000+i)
		if _, err := e.eng.Exec(e.ctx, q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSQLInsertBatch500(b *testing.B) {
	e := newBenchEnv(b, 10_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := "INSERT INTO bench VALUES "
		base := 2_000_000 + i*500
		for j := 0; j < 500; j++ {
			if j > 0 {
				q += ","
			}
			q += fmt.Sprintf("(%d,'x',1.0,7)", base+j)
		}
		if _, err := e.eng.Exec(e.ctx, q); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*500)/b.Elapsed().Seconds(), "rows/s")
}

func BenchmarkSQLPointSelect(b *testing.B) {
	e := newBenchEnv(b, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := fmt.Sprintf(`SELECT name, amount FROM bench WHERE id = %d`, i%100_000)
		rows, err := e.eng.Exec(e.ctx, q)
		if err != nil || len(rows) != 1 {
			b.Fatalf("%v %v", rows, err)
		}
	}
}

func BenchmarkSQLIndexRange1k(b *testing.B) {
	e := newBenchEnv(b, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := fmt.Sprintf(`SELECT COUNT(*) FROM bench WHERE grp = %d`, i%100)
		rows, err := e.eng.Exec(e.ctx, q)
		if err != nil || fmt.Sprint(rows[0][0]) != "1000" {
			b.Fatalf("%v %v", rows, err)
		}
	}
}

func BenchmarkSQLUpdateByPK(b *testing.B) {
	e := newBenchEnv(b, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		q := fmt.Sprintf(`UPDATE bench SET amount = amount + 1 WHERE id = %d`, i%100_000)
		if _, err := e.eng.Exec(e.ctx, q); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSQLVerifiedGet(b *testing.B) {
	e := newBenchEnv(b, 100_000)
	store := e.eng.Store()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		vr, err := store.VerifiedGet("bench", int64(i%100_000))
		if err != nil || vr.Row == nil {
			b.Fatal(err)
		}
		if ok, err := store.Verify(vr, &vr.Root); err != nil || !ok {
			b.Fatal(err)
		}
	}
}
