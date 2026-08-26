// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"bytes"
	"errors"
	"fmt"
	"testing"
)

var (
	testCK = [KeySize]byte{0x11, 0x22, 0x33, 0x44, 0x55, 0x66, 0x77, 0x88}
	testSK = [KeySize]byte{0x99, 0xAA, 0xBB, 0xCC, 0xDD, 0xEE, 0xFF, 0x00}
	altSK  = [KeySize]byte{0x01, 0x02, 0x03, 0x04}
)

func newTestStore(t *testing.T) (*Store, *MemBackend) {
	t.Helper()
	b := NewMemBackend()
	s, err := Create(b, testCK, testSK)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	return s, b
}

func mustPut(t *testing.T, s *Store, ops ...Op) (Hash, uint64) {
	t.Helper()
	root, version, err := s.PutBatch(ops)
	if err != nil {
		t.Fatalf("put batch: %v", err)
	}
	return root, version
}

func mustGet(t *testing.T, s *Store, key string) ([]byte, bool) {
	t.Helper()
	v, ok, err := s.Get([]byte(key))
	if err != nil {
		t.Fatalf("get %q: %v", key, err)
	}
	return v, ok
}

func expectValue(t *testing.T, s *Store, key, want string) {
	t.Helper()
	v, ok := mustGet(t, s, key)
	if !ok || !bytes.Equal(v, []byte(want)) {
		t.Fatalf("%q = %q (present=%v), want %q", key, v, ok, want)
	}
}

func expectAbsent(t *testing.T, s *Store, key string) {
	t.Helper()
	if v, ok := mustGet(t, s, key); ok {
		t.Fatalf("%q unexpectedly present: %q", key, v)
	}
}

func kindOf(t *testing.T, err error) Kind {
	t.Helper()
	var le *Error
	if !errors.As(err, &le) {
		t.Fatalf("expected ledger error, got %v", err)
	}
	return le.Kind
}

func TestEmptyStore(t *testing.T) {
	s, _ := newTestStore(t)
	root, version := s.Root()
	if root != Placeholder() || version != 0 {
		t.Fatalf("fresh store root/version = %x/%d", root, version)
	}
	expectAbsent(t, s, "absent")
}

func TestPutGetDelete(t *testing.T) {
	s, _ := newTestStore(t)
	root1, v1 := mustPut(t, s, Put([]byte("alice"), []byte("1000")), Put([]byte("bob"), []byte("250")))
	if v1 != 1 || root1 == Placeholder() {
		t.Fatalf("commit: root=%x version=%d", root1, v1)
	}
	expectValue(t, s, "alice", "1000")
	expectValue(t, s, "bob", "250")
	expectAbsent(t, s, "carol")

	root2, v2 := mustPut(t, s, Del([]byte("alice")))
	if v2 != 2 || root2 == root1 {
		t.Fatalf("delete did not advance state")
	}
	expectAbsent(t, s, "alice")

	// Deleting the last key returns the store to the placeholder root.
	root3, _ := mustPut(t, s, Del([]byte("bob")))
	if root3 != Placeholder() {
		t.Fatalf("empty store root = %x", root3)
	}
}

func TestEmptyValueIsAValue(t *testing.T) {
	s, _ := newTestStore(t)
	mustPut(t, s, Put([]byte("k"), []byte{}))
	v, ok, err := s.Get([]byte("k"))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !ok || len(v) != 0 {
		t.Fatalf("empty value came back as %v (present=%v)", v, ok)
	}
}

func TestBatchLastWinsAndNoop(t *testing.T) {
	s, _ := newTestStore(t)
	mustPut(t, s, Put([]byte("k"), []byte("first")), Put([]byte("k"), []byte("second")))
	if got, ok := mustGet(t, s, "k"); !ok || !bytes.Equal(got, []byte("second")) {
		t.Fatalf("k = %q, want last-wins", got)
	}

	root, version := s.Root()
	// Identical overwrite + delete of an absent key = no-op: no new version.
	r2, v2 := mustPut(t, s, Put([]byte("k"), []byte("second")), Del([]byte("absent")))
	if r2 != root || v2 != version {
		t.Fatalf("no-op batch advanced state: %x/%d -> %x/%d", root, version, r2, v2)
	}
}

func TestRootIsAPureFunctionOfContent(t *testing.T) {
	// The same logical content must produce the same root regardless of
	// the batch/order/deletion history that produced it.
	a, _ := newTestStore(t)
	b, _ := newTestStore(t)

	var ops []Op
	for i := 0; i < 60; i++ {
		ops = append(ops, Put([]byte(fmt.Sprintf("key-%02d", i)), []byte(fmt.Sprintf("val-%02d", i))))
	}
	mustPut(t, a, ops...)

	// Store b: reversed order, in many batches, with detours.
	mustPut(t, b, Put([]byte("transient"), []byte("x")))
	for i := 59; i >= 0; i-- {
		mustPut(t, b,
			Put([]byte(fmt.Sprintf("key-%02d", i)), []byte("wrong")),
			Put([]byte(fmt.Sprintf("key-%02d", i)), []byte(fmt.Sprintf("val-%02d", i))))
	}
	mustPut(t, b, Del([]byte("transient")))

	rootA, _ := a.Root()
	rootB, _ := b.Root()
	if rootA != rootB {
		t.Fatalf("same content, different roots: %x vs %x", rootA, rootB)
	}
}

func TestEncryptionIndependentRoot(t *testing.T) {
	// Same ck, different storage keys: identical roots.
	a, _ := newTestStore(t)
	bBackend := NewMemBackend()
	b, err := Create(bBackend, testCK, altSK)
	if err != nil {
		t.Fatal(err)
	}
	ops := []Op{Put([]byte("k1"), []byte("v1")), Put([]byte("k2"), []byte("v2"))}
	rootA, _, _ := a.PutBatch(ops)
	rootB, _, err := b.PutBatch(ops)
	if err != nil {
		t.Fatal(err)
	}
	if rootA != rootB {
		t.Fatalf("roots differ across storage keys: %x vs %x", rootA, rootB)
	}
}

func TestRestartFromCheckpoint(t *testing.T) {
	s, b := newTestStore(t)
	root, version := mustPut(t, s, Put([]byte("k"), []byte("v")))

	// OpenLatest resumes from the atomic checkpoint.
	s2, err := OpenLatest(b, testCK, testSK)
	if err != nil {
		t.Fatalf("open latest: %v", err)
	}
	r2, v2 := s2.Root()
	if r2 != root || v2 != version {
		t.Fatalf("restart state %x/%d, want %x/%d", r2, v2, root, version)
	}
	if got, ok := mustGet(t, s2, "k"); !ok || !bytes.Equal(got, []byte("v")) {
		t.Fatalf("k after restart = %q", got)
	}

	// Open at an explicit trusted checkpoint.
	s3, err := Open(b, testCK, testSK, root, version)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if got, ok := mustGet(t, s3, "k"); !ok || !bytes.Equal(got, []byte("v")) {
		t.Fatalf("k after explicit open = %q", got)
	}

	// Open at a root the backend does not hold fails closed.
	if _, err := Open(b, testCK, testSK, Hash{0xAB}, version); err == nil {
		t.Fatal("open at a forged root succeeded")
	}
}

func TestWrongStorageKeyFailsClosed(t *testing.T) {
	s, b := newTestStore(t)
	mustPut(t, s, Put([]byte("k"), []byte("v")))
	if _, err := OpenLatest(b, testCK, altSK); err == nil {
		t.Fatal("open with the wrong storage key succeeded")
	}
}

func TestTamperSweepFailsClosed(t *testing.T) {
	s, b := newTestStore(t)
	var ops []Op
	for i := 0; i < 20; i++ {
		ops = append(ops, Put([]byte(fmt.Sprintf("key-%d", i)), []byte(fmt.Sprintf("val-%d", i))))
	}
	mustPut(t, s, ops...)

	// Flip one bit of every record in turn; every read of every key must
	// then either succeed with the right value or fail closed — never
	// return wrong data.
	for _, recKey := range b.Keys() {
		if recKey[0] == recStale || recKey[0] == recCheckpoint || recKey[0] == recRoot {
			continue // not on the live read path
		}
		original, _, _ := b.Get(recKey)
		if !b.Tamper(recKey) {
			continue
		}
		s.SetCacheCapacity(defaultCacheCapacity) // clear the cache
		for i := 0; i < 20; i++ {
			key := []byte(fmt.Sprintf("key-%d", i))
			want := []byte(fmt.Sprintf("val-%d", i))
			got, _, err := s.Get(key)
			if err == nil && !bytes.Equal(got, want) {
				t.Fatalf("tampered record %x: key-%d returned wrong data %q", recKey, i, got)
			}
		}
		_ = b.WriteBatch([]BatchOp{{Key: recKey, Value: original}})
	}
}

func TestMissingRecordFailsClosed(t *testing.T) {
	s, b := newTestStore(t)
	mustPut(t, s, Put([]byte("k"), []byte("v")))
	// Remove the value record.
	for _, recKey := range b.Keys() {
		if recKey[0] == recValue {
			b.Remove(recKey)
		}
	}
	s.SetCacheCapacity(defaultCacheCapacity)
	_, _, err := s.Get([]byte("k"))
	if err == nil {
		t.Fatal("read of a lost value succeeded")
	}
	if k := kindOf(t, err); k != KindMissing && k != KindCorrupted {
		t.Fatalf("unexpected error kind %v", k)
	}
}

func TestHistory(t *testing.T) {
	s, _ := newTestStore(t)
	root1, v1 := mustPut(t, s, Put([]byte("k"), []byte("one")))
	root2, v2 := mustPut(t, s, Put([]byte("k"), []byte("two")))

	if r, err := s.RootAt(v1); err != nil || r != root1 {
		t.Fatalf("root at v1 = %x, %v", r, err)
	}
	if r, err := s.RootAt(v2); err != nil || r != root2 {
		t.Fatalf("root at v2 = %x, %v", r, err)
	}
	if v, ok, err := s.GetAt(v1, []byte("k")); err != nil || !ok || !bytes.Equal(v, []byte("one")) {
		t.Fatalf("get at v1 = %q, %v", v, err)
	}
	if v, ok, err := s.GetAt(v2, []byte("k")); err != nil || !ok || !bytes.Equal(v, []byte("two")) {
		t.Fatalf("get at v2 = %q, %v", v, err)
	}
	if _, _, err := s.GetAt(v2+1, []byte("k")); err == nil {
		t.Fatal("future version read succeeded")
	}
}

func TestPrune(t *testing.T) {
	s, _ := newTestStore(t)
	_, v1 := mustPut(t, s, Put([]byte("k"), []byte("one")))
	_, v2 := mustPut(t, s, Put([]byte("k"), []byte("two")))
	_, v3 := mustPut(t, s, Put([]byte("k"), []byte("three")))

	stats, err := s.Prune(v3)
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if stats.RecordsDeleted == 0 || stats.RootRecordsDeleted == 0 {
		t.Fatalf("prune deleted nothing: %+v", stats)
	}

	// Live state and the horizon version stay readable.
	if got, ok := mustGet(t, s, "k"); !ok || !bytes.Equal(got, []byte("three")) {
		t.Fatalf("live read after prune = %q", got)
	}
	if v, ok, err := s.GetAt(v3, []byte("k")); err != nil || !ok || !bytes.Equal(v, []byte("three")) {
		t.Fatalf("get at horizon = %q, %v", v, err)
	}
	// History below the horizon is gone, cleanly.
	if _, _, err := s.GetAt(v1, []byte("k")); err == nil {
		t.Fatal("pruned version still readable")
	}
	if _, err := s.RootAt(v2); err == nil {
		t.Fatal("pruned root record still readable")
	}

	// Idempotent.
	if _, err := s.Prune(v3); err != nil {
		t.Fatalf("re-prune: %v", err)
	}
}

func TestPinBlocksPruning(t *testing.T) {
	s, _ := newTestStore(t)
	_, v1 := mustPut(t, s, Put([]byte("k"), []byte("one")))
	mustPut(t, s, Put([]byte("k"), []byte("two")))

	pin := v1
	s.PinVersion(&pin)
	if _, err := s.Prune(v1 + 1); err != nil {
		t.Fatalf("prune with pin: %v", err)
	}
	if v, ok, err := s.GetAt(v1, []byte("k")); err != nil || !ok || !bytes.Equal(v, []byte("one")) {
		t.Fatalf("pinned version pruned away: %q, %v", v, err)
	}
	s.PinVersion(nil)
	if _, err := s.Prune(v1 + 1); err != nil {
		t.Fatalf("prune after unpin: %v", err)
	}
	if _, _, err := s.GetAt(v1, []byte("k")); err == nil {
		t.Fatal("unpinned version survived pruning")
	}
}

func TestWarmReadIOBudget(t *testing.T) {
	s, b := newTestStore(t)
	var ops []Op
	for i := 0; i < 2000; i++ {
		ops = append(ops, Put([]byte(fmt.Sprintf("key-%05d", i)), []byte("v")))
	}
	mustPut(t, s, ops...)

	mustGet(t, s, "key-01000") // warm the path
	b.ResetReads()
	mustGet(t, s, "key-01000")
	if r := b.Reads(); r > 4 {
		t.Fatalf("warm verified read cost %d backend reads", r)
	}
}

func TestSnapshotExportRestore(t *testing.T) {
	s, _ := newTestStore(t)
	var ops []Op
	for i := 0; i < 40; i++ {
		ops = append(ops, Put([]byte(fmt.Sprintf("key-%02d", i)), []byte(fmt.Sprintf("val-%02d", i))))
	}
	root, version := mustPut(t, s, ops...)

	// Chunked export.
	var all []Leaf
	var startAfter *Hash
	for {
		leaves, done, err := s.SnapshotLeaves(version, startAfter, 7)
		if err != nil {
			t.Fatalf("snapshot: %v", err)
		}
		all = append(all, leaves...)
		if done {
			break
		}
		last := leaves[len(leaves)-1].Path
		startAfter = &last
	}
	if len(all) != 40 {
		t.Fatalf("exported %d leaves, want 40", len(all))
	}

	// Restore into a store with a DIFFERENT storage key: same root.
	rb := NewMemBackend()
	restored, err := Create(rb, testCK, altSK)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := restored.RestoreLeaves(all); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if err := restored.StampVersion(version); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	rRoot, rVersion := restored.Root()
	if rRoot != root || rVersion != version {
		t.Fatalf("restored %x/%d, want %x/%d", rRoot, rVersion, root, version)
	}
	// And the restored store reads logical keys normally.
	if got, ok := mustGet(t, restored, "key-17"); !ok || !bytes.Equal(got, []byte("val-17")) {
		t.Fatalf("restored key-17 = %q", got)
	}
}

func TestFork(t *testing.T) {
	s, _ := newTestStore(t)
	mustPut(t, s, Put([]byte("balance"), []byte("100")))

	f := NewFork(s)
	if v, ok, _ := f.Get([]byte("balance")); !ok || !bytes.Equal(v, []byte("100")) {
		t.Fatalf("fork read-through = %q", v)
	}
	f.Put([]byte("balance"), []byte("90"))
	f.Put([]byte("fee"), []byte("10"))
	f.Delete([]byte("absent"))
	if v, ok, _ := f.Get([]byte("balance")); !ok || !bytes.Equal(v, []byte("90")) {
		t.Fatalf("fork overlay read = %q", v)
	}

	sealed, err := f.Seal()
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if sealed.IsNoop() {
		t.Fatal("effective transaction sealed as no-op")
	}
	// Nothing persisted yet.
	if got, ok := mustGet(t, s, "balance"); !ok || !bytes.Equal(got, []byte("100")) {
		t.Fatalf("store mutated by fork: %q", got)
	}
	// Applying the sealed write-set produces exactly the previewed root.
	root, version := mustPut(t, s, sealed.Ops...)
	if root != sealed.RootAfter || version != sealed.VersionAfter {
		t.Fatalf("applied %x/%d, sealed %x/%d", root, version, sealed.RootAfter, sealed.VersionAfter)
	}
	if got, ok := mustGet(t, s, "fee"); !ok || !bytes.Equal(got, []byte("10")) {
		t.Fatalf("fee = %q", got)
	}
}

func TestForkSealFailsIfStoreMoved(t *testing.T) {
	s, _ := newTestStore(t)
	f := NewFork(s)
	f.Put([]byte("k"), []byte("v"))
	mustPut(t, s, Put([]byte("other"), []byte("x")))
	if _, err := f.Seal(); err == nil {
		t.Fatal("seal succeeded after the store moved")
	}
}

func TestCacheDisable(t *testing.T) {
	s, b := newTestStore(t)
	mustPut(t, s, Put([]byte("k"), []byte("v")))
	s.SetCacheCapacity(0)
	mustGet(t, s, "k")
	b.ResetReads()
	mustGet(t, s, "k")
	if b.Reads() == 0 {
		t.Fatal("cache disabled but no backend reads")
	}
}
