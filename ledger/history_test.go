// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"fmt"
	"testing"
)

var histCK = [KeySize]byte{0xA1}

func chainedStore(t *testing.T) (*MemBackend, *Store) {
	t.Helper()
	be := NewMemBackend()
	s, err := Create(be, histCK, WithHistoryChain())
	if err != nil {
		t.Fatal(err)
	}
	return be, s
}

func histPut(t *testing.T, s *Store, kv ...string) (Hash, uint64) {
	t.Helper()
	ops := make([]Op, 0, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		ops = append(ops, Put([]byte(kv[i]), []byte(kv[i+1])))
	}
	root, version, err := s.PutBatch(ops)
	if err != nil {
		t.Fatal(err)
	}
	return root, version
}

func TestHistoryChainBasics(t *testing.T) {
	_, s := chainedStore(t)

	var roots []Hash
	r0, _ := s.Root()
	roots = append(roots, r0)
	for i := 1; i <= 5; i++ {
		r, v := histPut(t, s, fmt.Sprintf("k%d", i), fmt.Sprintf("v%d", i))
		if v != uint64(i) {
			t.Fatalf("version %d != %d", v, i)
		}
		roots = append(roots, r)
	}

	// The live head equals the chain recomputed from the roots.
	want := Hash{}
	for v := uint64(1); v <= 5; v++ {
		want = HistoryLink(want, roots[v-1], v)
	}
	head, hv, err := s.HistoryHead()
	if err != nil || hv != 5 || head != want {
		t.Fatalf("head mismatch: %x/%d vs %x (%v)", head, hv, want, err)
	}

	// The head is ordinary, provable state.
	val, ok, err := s.Get(HistoryKey)
	if err != nil || !ok || string(val) != string(head[:]) {
		t.Fatalf("head leaf read: %v %v", ok, err)
	}
	proof, err := s.Prove(HistoryKey)
	if err != nil {
		t.Fatal(err)
	}
	root, _ := s.Root()
	if ok, err := s.VerifyValue(&root, HistoryKey, head[:], proof); err != nil || !ok {
		t.Fatalf("head proof: %v %v", ok, err)
	}

	// Full and incremental verification.
	if err := s.VerifyHistory(0, Hash{}); err != nil {
		t.Fatalf("verify from genesis: %v", err)
	}
	midHead, err := s.HistoryHeadAt(3)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyHistory(3, midHead); err != nil {
		t.Fatalf("verify from anchor: %v", err)
	}
	if err := s.VerifyHistory(5, head); err != nil {
		t.Fatalf("verify at tip: %v", err)
	}
	// A wrong anchor fails.
	if err := s.VerifyHistory(3, Hash{0xFF}); err == nil {
		t.Fatal("verify accepted a wrong anchor")
	}

	// No-op batches stay no-ops (no version churn from the chain).
	rBefore, vBefore := s.Root()
	r, v, err := s.PutBatch([]Op{Put([]byte("k1"), []byte("v1"))})
	if err != nil || r != rBefore || v != vBefore {
		t.Fatalf("no-op batch committed: %v", err)
	}

	// The reserved namespace is not writable.
	if _, _, err := s.PutBatch([]Op{Put(HistoryKey, []byte("x"))}); err == nil {
		t.Fatal("write to reserved key accepted")
	}
	if _, _, err := s.PutBatch([]Op{Del(HistoryKey)}); err == nil {
		t.Fatal("delete of reserved key accepted")
	}
}

func TestHistoryChainReopen(t *testing.T) {
	be, s := chainedStore(t)
	histPut(t, s, "a", "1", "b", "2")
	histPut(t, s, "a", "3")
	head, _, _ := s.HistoryHead()
	root, version := s.Root()

	// OpenLatest restores mode and head from the checkpoint.
	s2, err := OpenLatest(be, histCK)
	if err != nil {
		t.Fatal(err)
	}
	if !s2.HistoryEnabled() {
		t.Fatal("chain mode lost on reopen")
	}
	head2, _, _ := s2.HistoryHead()
	if head2 != head {
		t.Fatal("head lost on reopen")
	}
	if err := s2.VerifyHistory(0, Hash{}); err != nil {
		t.Fatal(err)
	}

	// Explicit-anchor open, and with the checkpoint stripped: the
	// anchored root itself proves the chain leaf, so mode survives.
	s3, err := Open(be, histCK, root, version)
	if err != nil || !s3.HistoryEnabled() {
		t.Fatalf("explicit open: %v", err)
	}
	be.Remove(checkpointRecordKey())
	s4, err := Open(be, histCK, root, version)
	if err != nil || !s4.HistoryEnabled() {
		t.Fatalf("explicit open without checkpoint: %v", err)
	}

	// Asserting the chain on a store that never had one fails.
	be2 := NewMemBackend()
	if _, err := Create(be2, histCK); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLatest(be2, histCK, WithHistoryChain()); err == nil {
		t.Fatal("chain assertion on a chainless store accepted")
	}
}

func TestHistoryChainTamperDetection(t *testing.T) {
	be, s := chainedStore(t)
	for i := 1; i <= 4; i++ {
		histPut(t, s, fmt.Sprintf("k%d", i), "v")
	}
	// Corrupt one historical root record.
	be.Tamper(rootRecordKey(2))
	if err := s.VerifyHistory(0, Hash{}); err == nil {
		t.Fatal("verify accepted a rewritten root record")
	}
}

func TestHistoryChainPruneKeepsAnchoredSuffix(t *testing.T) {
	_, s := chainedStore(t)
	for i := 1; i <= 6; i++ {
		histPut(t, s, fmt.Sprintf("k%d", i), "v", "k1", fmt.Sprintf("rewrite%d", i))
	}
	anchorHead, err := s.HistoryHeadAt(4)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Prune(4); err != nil {
		t.Fatal(err)
	}
	if err := s.VerifyHistory(4, anchorHead); err != nil {
		t.Fatalf("verify from post-prune anchor: %v", err)
	}
	if err := s.VerifyHistory(0, Hash{}); err == nil {
		t.Fatal("verify across the pruned range succeeded")
	}
	// Live state survives pruning intact (the chain leaf is staled and
	// rewritten every commit; a mis-staled live record would fail here).
	for i := 1; i <= 6; i++ {
		v, ok, err := s.Get([]byte(fmt.Sprintf("k%d", i)))
		if err != nil || !ok {
			t.Fatalf("k%d after prune: %v %v", i, ok, err)
		}
		want := "v"
		if i == 1 {
			want = "rewrite6"
		}
		if string(v) != want {
			t.Fatalf("k%d after prune: %q", i, v)
		}
	}
	if err := s.VerifyHistory(6, mustHead(t, s)); err != nil {
		t.Fatal(err)
	}
}

func mustHead(t *testing.T, s *Store) Hash {
	t.Helper()
	h, _, err := s.HistoryHead()
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestHistoryChainDeterministicAndForkConsistent(t *testing.T) {
	run := func() (*Store, Hash, Hash) {
		_, s := chainedStore(t)
		histPut(t, s, "a", "1", "b", "2")
		histPut(t, s, "b", "3", "c", "4")
		r, _ := s.Root()
		h, _, _ := s.HistoryHead()
		return s, r, h
	}
	s1, r1, h1 := run()
	_, r2, h2 := run()
	if r1 != r2 || h1 != h2 {
		t.Fatal("identical histories diverged")
	}

	// A sealed fork previews exactly the root the commit produces,
	// chain leaf included.
	f := NewFork(s1)
	f.Put([]byte("d"), []byte("5"))
	f.Delete([]byte("a"))
	sf, err := f.Seal()
	if err != nil {
		t.Fatal(err)
	}
	root, version, err := s1.PutBatch(sf.Ops)
	if err != nil {
		t.Fatal(err)
	}
	if root != sf.RootAfter || version != sf.VersionAfter {
		t.Fatalf("fork preview diverged from commit: %x != %x", root, sf.RootAfter)
	}
}

func TestChangesAt(t *testing.T) {
	_, s := chainedStore(t)
	histPut(t, s, "keep", "same", "change", "old", "drop", "bye")
	if _, _, err := s.PutBatch([]Op{
		Put([]byte("change"), []byte("new")),
		Put([]byte("add"), []byte("hello")),
		Del([]byte("drop")),
	}); err != nil {
		t.Fatal(err)
	}

	changes, err := s.ChangesAt(2)
	if err != nil {
		t.Fatal(err)
	}
	got := make(map[Hash]*Change, len(changes))
	for i := range changes {
		got[changes[i].Path] = &changes[i]
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 changes, got %d: %+v", len(got), changes)
	}
	check := func(key, want string, deleted bool) {
		t.Helper()
		c, ok := got[s.pathOf([]byte(key))]
		if !ok {
			t.Fatalf("change for %q missing", key)
		}
		if c.Deleted != deleted || (!deleted && string(c.Value) != want) {
			t.Fatalf("change for %q: %+v", key, c)
		}
	}
	check("change", "new", false)
	check("add", "hello", false)
	check("drop", "", true)
	if _, ok := got[s.pathOf([]byte("keep"))]; ok {
		t.Fatal("unchanged key reported")
	}
	if _, ok := got[s.pathOf(HistoryKey)]; ok {
		t.Fatal("history leaf reported as a change")
	}
}

func TestHistoryChainSnapshotTransfer(t *testing.T) {
	_, src := chainedStore(t)
	for i := 1; i <= 3; i++ {
		histPut(t, src, fmt.Sprintf("k%d", i), "v")
	}
	srcRoot, srcVersion := src.Root()
	srcHead, _, _ := src.HistoryHead()

	leaves, done, err := src.SnapshotLeaves(srcVersion, nil, 1<<20)
	if err != nil || !done {
		t.Fatal(err)
	}
	be2 := NewMemBackend()
	dst, err := Create(be2, histCK, WithHistoryChain())
	if err != nil {
		t.Fatal(err)
	}
	root, _, err := dst.RestoreLeaves(leaves)
	if err != nil {
		t.Fatal(err)
	}
	if root != srcRoot {
		t.Fatal("restored root differs")
	}
	if err := dst.StampVersion(srcVersion); err != nil {
		t.Fatal(err)
	}
	// The chain continues seamlessly from the transferred anchor.
	histPut(t, dst, "k4", "v")
	if err := dst.VerifyHistory(srcVersion, srcHead); err != nil {
		t.Fatalf("chain continuity across transfer: %v", err)
	}
}
