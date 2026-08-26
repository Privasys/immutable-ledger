// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package pebbleback

import (
	"bytes"
	"fmt"
	"testing"

	ledger "github.com/Privasys/immutable-ledger"
)

func openTest(t *testing.T) *Backend {
	t.Helper()
	b, err := Open(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func TestBackendContract(t *testing.T) {
	b := openTest(t)

	// Absent key.
	if _, ok, err := b.Get([]byte("nope")); err != nil || ok {
		t.Fatalf("absent get = %v ok=%v", err, ok)
	}

	// Atomic batch with puts and deletes.
	err := b.WriteBatch([]ledger.BatchOp{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
		{Key: []byte("c"), Value: []byte("3")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := b.Get([]byte("b")); !ok || !bytes.Equal(v, []byte("2")) {
		t.Fatalf("b = %q ok=%v", v, ok)
	}
	if err := b.WriteBatch([]ledger.BatchOp{
		{Key: []byte("b"), Delete: true},
		{Key: []byte("d"), Value: []byte("4")},
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := b.Get([]byte("b")); ok {
		t.Fatal("deleted key still present")
	}

	// Ordered scan with bounds and limit.
	kvs, err := b.Scan([]byte("a"), []byte("d"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(kvs) != 2 || string(kvs[0].Key) != "a" || string(kvs[1].Key) != "c" {
		t.Fatalf("scan = %v", kvs)
	}
	kvs, _ = b.Scan([]byte("a"), nil, 2)
	if len(kvs) != 2 {
		t.Fatalf("limited scan = %d entries", len(kvs))
	}
	// Empty-value records survive.
	if err := b.WriteBatch([]ledger.BatchOp{{Key: []byte("e")}}); err != nil {
		t.Fatal(err)
	}
	if v, ok, _ := b.Get([]byte("e")); !ok || len(v) != 0 {
		t.Fatalf("empty record = %q ok=%v", v, ok)
	}
}

func TestLedgerOverPebble(t *testing.T) {
	var ck [ledger.KeySize]byte
	ck[0] = 0x42
	dir := t.TempDir()

	b, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := ledger.Create(b, ck)
	if err != nil {
		t.Fatal(err)
	}
	var ops []ledger.Op
	for i := 0; i < 200; i++ {
		ops = append(ops, ledger.Put(
			[]byte(fmt.Sprintf("key-%03d", i)),
			[]byte(fmt.Sprintf("val-%03d", i))))
	}
	root, version, err := s.PutBatch(ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}

	// Reopen from disk: checkpoint resumes, data verifies.
	b2, err := Open(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b2.Close()
	s2, err := ledger.OpenLatest(b2, ck)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	r2, v2 := s2.Root()
	if r2 != root || v2 != version {
		t.Fatalf("reopened %x/%d, want %x/%d", r2, v2, root, version)
	}
	v, ok, err := s2.Get([]byte("key-123"))
	if err != nil || !ok || !bytes.Equal(v, []byte("val-123")) {
		t.Fatalf("key-123 = %q ok=%v err=%v", v, ok, err)
	}

	// Proofs work over the disk-backed store.
	proof, err := s2.Prove([]byte("key-123"))
	if err != nil {
		t.Fatal(err)
	}
	okp, err := s2.VerifyValue(&r2, []byte("key-123"), []byte("val-123"), proof)
	if err != nil || !okp {
		t.Fatalf("proof over pebble failed: %v", err)
	}
}
