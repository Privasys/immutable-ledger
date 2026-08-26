// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package pebbleback

import (
	"fmt"
	"testing"

	ledger "github.com/Privasys/immutable-ledger/ledger"
)

// Disk-backed benchmarks: the ledger over Pebble, synced batches
// (default durability) and unsynced for comparison.

var benchCK = [ledger.KeySize]byte{0xB0}

func benchLedger(b *testing.B, noSync bool, prefill int) *ledger.Store {
	b.Helper()
	be, err := Open(b.TempDir(), &Options{NoSync: noSync})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = be.Close() })
	s, err := ledger.Create(be, benchCK)
	if err != nil {
		b.Fatal(err)
	}
	batch := make([]ledger.Op, 0, 1000)
	for i := 0; i < prefill; i++ {
		batch = append(batch, ledger.Put(
			[]byte(fmt.Sprintf("key-%09d", i)),
			[]byte(fmt.Sprintf("value-%09d-0123456789abcdef0123456789abcdef", i))))
		if len(batch) == 1000 {
			if _, _, err := s.PutBatch(batch); err != nil {
				b.Fatal(err)
			}
			batch = batch[:0]
		}
	}
	if len(batch) > 0 {
		if _, _, err := s.PutBatch(batch); err != nil {
			b.Fatal(err)
		}
	}
	return s
}

func benchmarkPut(b *testing.B, noSync bool, batchSize int) {
	s := benchLedger(b, noSync, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ops := make([]ledger.Op, 0, batchSize)
		for j := 0; j < batchSize; j++ {
			ops = append(ops, ledger.Put(
				[]byte(fmt.Sprintf("bench-%d-%d", i, j)),
				[]byte("value-0123456789abcdef0123456789abcdef")))
		}
		if _, _, err := s.PutBatch(ops); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*batchSize)/b.Elapsed().Seconds(), "rows/s")
}

func BenchmarkPutBatch1Synced(b *testing.B)     { benchmarkPut(b, false, 1) }
func BenchmarkPutBatch100Synced(b *testing.B)   { benchmarkPut(b, false, 100) }
func BenchmarkPutBatch1000Synced(b *testing.B)  { benchmarkPut(b, false, 1000) }
func BenchmarkPutBatch100Unsynced(b *testing.B) { benchmarkPut(b, true, 100) }

func BenchmarkGetWarmAt100k(b *testing.B) {
	s := benchLedger(b, false, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := []byte(fmt.Sprintf("key-%09d", i%100_000))
		if _, ok, err := s.Get(key); err != nil || !ok {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetColdAt100k(b *testing.B) {
	s := benchLedger(b, false, 100_000)
	s.SetCacheCapacity(0)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := []byte(fmt.Sprintf("key-%09d", (i*7919)%100_000))
		if _, ok, err := s.Get(key); err != nil || !ok {
			b.Fatal(err)
		}
	}
}

func BenchmarkProveAt100k(b *testing.B) {
	s := benchLedger(b, false, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Prove([]byte(fmt.Sprintf("key-%09d", i%100_000))); err != nil {
			b.Fatal(err)
		}
	}
}
