// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"fmt"
	"testing"
)

func TestProofInclusionAndAbsence(t *testing.T) {
	s, _ := newTestStore(t)
	var ops []Op
	for i := 0; i < 50; i++ {
		ops = append(ops, Put([]byte(fmt.Sprintf("key-%02d", i)), []byte(fmt.Sprintf("val-%02d", i))))
	}
	root, _ := mustPut(t, s, ops...)

	for i := 0; i < 50; i++ {
		key := []byte(fmt.Sprintf("key-%02d", i))
		proof, err := s.Prove(key)
		if err != nil {
			t.Fatalf("prove %s: %v", key, err)
		}
		ok, err := s.VerifyValue(&root, key, []byte(fmt.Sprintf("val-%02d", i)), proof)
		if err != nil || !ok {
			t.Fatalf("inclusion proof for %s failed: %v", key, err)
		}
		// The same proof does not verify a different value.
		ok, err = s.VerifyValue(&root, key, []byte("forged"), proof)
		if err != nil || ok {
			t.Fatalf("proof verified a forged value for %s", key)
		}
	}

	// Absence.
	absent := []byte("no-such-key")
	proof, err := s.Prove(absent)
	if err != nil {
		t.Fatalf("prove absent: %v", err)
	}
	ok, err := s.VerifyAbsent(&root, absent, proof)
	if err != nil || !ok {
		t.Fatalf("absence proof failed: %v", err)
	}
	// An absence proof is not an inclusion proof.
	if ok, _ := s.VerifyValue(&root, absent, []byte("x"), proof); ok {
		t.Fatal("absence proof verified a value")
	}
}

func TestProofAgainstWrongRootFails(t *testing.T) {
	s, _ := newTestStore(t)
	root1, _ := mustPut(t, s, Put([]byte("k"), []byte("one")))
	proof, err := s.Prove([]byte("k"))
	if err != nil {
		t.Fatal(err)
	}
	mustPut(t, s, Put([]byte("k"), []byte("two")))
	root2, _ := s.Root()

	// The old proof holds against the old root, not the new one.
	if ok, err := s.VerifyValue(&root1, []byte("k"), []byte("one"), proof); err != nil || !ok {
		t.Fatalf("proof against its own root failed: %v", err)
	}
	if _, err := Verify(&root2, hashOf(s, "k"), proof); err == nil {
		t.Fatal("stale proof verified against the new root")
	}
}

func hashOf(s *Store, key string) *Hash {
	p := s.pathOf([]byte(key))
	return &p
}

func TestProofCodecRoundtrip(t *testing.T) {
	s, _ := newTestStore(t)
	root, _ := mustPut(t, s, Put([]byte("a"), []byte("1")), Put([]byte("b"), []byte("2")), Put([]byte("c"), []byte("3")))
	for _, key := range []string{"a", "b", "c", "absent"} {
		proof, err := s.Prove([]byte(key))
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeProof(proof.Encode())
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if _, err := Verify(&root, hashOf(s, key), decoded); err != nil {
			t.Fatalf("decoded proof for %q does not verify: %v", key, err)
		}
	}
}

func TestProofDecodeRejectsGarbage(t *testing.T) {
	for _, data := range [][]byte{{}, {2}, {1, 0, 0}} {
		if _, err := DecodeProof(data); err == nil {
			t.Fatalf("decoded garbage %v", data)
		}
	}
	s, _ := newTestStore(t)
	mustPut(t, s, Put([]byte("a"), []byte("1")))
	good, _ := s.Prove([]byte("a"))
	enc := good.Encode()
	if _, err := DecodeProof(enc[:len(enc)-1]); err == nil {
		t.Fatal("decoded a truncated proof")
	}
	if _, err := DecodeProof(append(append([]byte(nil), enc...), 0)); err == nil {
		t.Fatal("decoded a proof with trailing bytes")
	}
}

func TestHistoricalProofs(t *testing.T) {
	s, _ := newTestStore(t)
	root1, v1 := mustPut(t, s, Put([]byte("k"), []byte("one")))
	mustPut(t, s, Put([]byte("k"), []byte("two")))

	proof, err := s.ProveAt(v1, []byte("k"))
	if err != nil {
		t.Fatalf("prove at: %v", err)
	}
	ok, err := s.VerifyValue(&root1, []byte("k"), []byte("one"), proof)
	if err != nil || !ok {
		t.Fatalf("historical proof failed: %v", err)
	}
}

func TestEmptyTreeAbsenceProof(t *testing.T) {
	s, _ := newTestStore(t)
	proof, err := s.Prove([]byte("anything"))
	if err != nil {
		t.Fatal(err)
	}
	root := Placeholder()
	ok, err := s.VerifyAbsent(&root, []byte("anything"), proof)
	if err != nil || !ok {
		t.Fatalf("empty-tree absence proof failed: %v", err)
	}
}
