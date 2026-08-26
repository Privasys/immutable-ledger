// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"fmt"
	"testing"
)

// Core-store benchmarks over the in-memory backend (pure CPU + codec
// cost; run the backend/pebble benchmarks for disk-backed figures).

func benchStore(b *testing.B, prefill int) *Store {
	b.Helper()
	s, err := Create(NewMemBackend(), testCK)
	if err != nil {
		b.Fatal(err)
	}
	batch := make([]Op, 0, 1000)
	for i := 0; i < prefill; i++ {
		batch = append(batch, Put(
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

func BenchmarkPutBatch100At100k(b *testing.B) {
	s := benchStore(b, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var ops []Op
		for j := 0; j < 100; j++ {
			ops = append(ops, Put(
				[]byte(fmt.Sprintf("bench-%d-%d", i, j)),
				[]byte("value-0123456789abcdef0123456789abcdef")))
		}
		if _, _, err := s.PutBatch(ops); err != nil {
			b.Fatal(err)
		}
	}
	b.ReportMetric(float64(b.N*100)/b.Elapsed().Seconds(), "rows/s")
}

func BenchmarkGetWarmAt100k(b *testing.B) {
	s := benchStore(b, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		key := []byte(fmt.Sprintf("key-%09d", i%100_000))
		if _, ok, err := s.Get(key); err != nil || !ok {
			b.Fatal(err)
		}
	}
}

func BenchmarkProveAt100k(b *testing.B) {
	s := benchStore(b, 100_000)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Prove([]byte(fmt.Sprintf("key-%09d", i%100_000))); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkVerifyProof(b *testing.B) {
	s := benchStore(b, 100_000)
	root, _ := s.Root()
	proof, err := s.Prove([]byte("key-000012345"))
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ok, err := s.VerifyValue(&root, []byte("key-000012345"),
			[]byte("value-000012345-0123456789abcdef0123456789abcdef"), proof)
		if err != nil || !ok {
			b.Fatal(err)
		}
	}
}
