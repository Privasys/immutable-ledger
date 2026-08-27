// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

// Auditable history.
//
// # The history chain (WithHistoryChain)
//
// Every effective commit writes one reserved leaf, HistoryKey, holding
// the chain head
//
//	head_v = SHA-256("immutable-ledger:history:v1" ‖ head_{v-1}
//	                 ‖ root_{v-1} ‖ v u64 BE)
//
// with head_0 = 32 zero bytes and root_0 the empty-tree placeholder
// root. Because the head is a leaf, root_v commits to head_v, which
// commits to every root before it: the live root pins the entire root
// lineage. Storage cannot rewrite history between two audits and stay
// consistent with the current root — fabricating an alternative
// sequence that ends at the same head is a preimage attack.
//
// # The audit workflow (owner-side or delegated: requires ck)
//
// At each audit, from the previous audit's signed anchor
// (version_A, root_A, head_A):
//
//  1. VerifyHistory(version_A, head_A) — walks the recorded roots and
//     confirms the chain ends at the live, root-bound head. This
//     proves the recorded root sequence is the unique lineage from
//     the anchor to the current state.
//  2. Review content as needed: ChangesAt(v) extracts what each
//     version changed (tree diff, cost proportional to the change),
//     and proofs cover individual keys.
//  3. Sign the new anchor (version_B, root_B, head_B = HistoryHead).
//  4. Prune(version_B) — the audited history, including every
//     superseded and deleted value, is physically removed; the signed
//     anchor stands in for it from then on.
//
// Between audits, deleted data persists in pruned-able history; full
// removal happens at the next audit's prune, so the audit cadence
// bounds the erasure latency.

// HistoryKey is the reserved logical key of the history-chain head
// leaf. It lives in the reserved system namespace: user batches cannot
// write it; reads return the current head.
var HistoryKey = []byte("\x00immutable-ledger:history-head")

var historyChainTag = []byte("immutable-ledger:history:v1")

// HistoryLink computes the chain head written by the commit that
// produced version, from the previous head and previous root. Pure
// function; head_0 is all zeros.
func HistoryLink(prevHead, prevRoot Hash, version uint64) Hash {
	h := sha256.New()
	h.Write(historyChainTag)
	h.Write(prevHead[:])
	h.Write(prevRoot[:])
	var v [8]byte
	binary.BigEndian.PutUint64(v[:], version)
	h.Write(v[:])
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

// HistoryEnabled reports whether this store maintains the chain.
func (s *Store) HistoryEnabled() bool { return s.history }

// HistoryHead returns the current chain head and the version it covers
// (zeros before the first chained commit). Anchor all three of
// (root, version, head) at each audit.
func (s *Store) HistoryHead() (Hash, uint64, error) {
	if !s.history {
		return Hash{}, 0, errInvalid("history chain is not enabled on this store")
	}
	return s.historyHead, s.version, nil
}

// HistoryHeadAt returns the chain head recorded at a historical
// version (the same freshness caveat as GetAt; version 0 is the
// all-zero genesis head).
func (s *Store) HistoryHeadAt(version uint64) (Hash, error) {
	if !s.history {
		return Hash{}, errInvalid("history chain is not enabled on this store")
	}
	if version == 0 {
		return Hash{}, nil
	}
	val, ok, err := s.GetAt(version, HistoryKey)
	if err != nil {
		return Hash{}, err
	}
	if !ok || len(val) != HashSize {
		return Hash{}, errCorruptedf("history head leaf missing or malformed at version %d", version)
	}
	var out Hash
	copy(out[:], val)
	return out, nil
}

// VerifyHistory confirms that the recorded root sequence from
// (fromVersion, fromHead) to the live state is the unique lineage the
// current root commits to: it recomputes the chain over the stored
// root records and compares the result with the live, root-bound head.
// fromVersion 0 with a zero fromHead verifies from genesis; otherwise
// pass a previously anchored (version, head) pair. Fails if any root
// record in the range was pruned — audit before pruning.
func (s *Store) VerifyHistory(fromVersion uint64, fromHead Hash) error {
	if !s.history {
		return errInvalid("history chain is not enabled on this store")
	}
	if fromVersion > s.version {
		return errInvalidf("version %d is in the future (current %d)", fromVersion, s.version)
	}
	head := fromHead
	for v := fromVersion + 1; v <= s.version; v++ {
		prevRoot, err := s.loadRootRecord(v - 1)
		if err != nil {
			return fmt.Errorf("history walk at version %d (pruned?): %w", v-1, err)
		}
		head = HistoryLink(head, prevRoot, v)
	}
	if head != s.historyHead {
		return errCorruptedf(
			"history chain mismatch: recorded roots from version %d do not produce the live head", fromVersion)
	}
	return nil
}

// -- transition extraction -------------------------------------------------

// Change is one leaf-level difference between a version and its
// predecessor. Path is the tree position (the keyed hash of the
// logical key — logical keys are not recoverable); Value is the
// plaintext for puts and nil for deletes.
type Change struct {
	Path    Hash
	Value   []byte
	Deleted bool
}

// ChangesAt extracts what the commit producing version changed,
// by structural diff against the predecessor: subtrees with equal
// hashes are skipped, so the cost is proportional to the change, not
// the store. The reserved history leaf is omitted (verify it with
// VerifyHistory instead). Works on any store, chained or not, for any
// unpruned version.
func (s *Store) ChangesAt(version uint64) ([]Change, error) {
	if version == 0 || version > s.version {
		return nil, errInvalidf("version %d out of range (current %d)", version, s.version)
	}
	oldRoot, err := s.loadRootRecord(version - 1)
	if err != nil {
		return nil, err
	}
	newRoot, err := s.loadRootRecord(version)
	if err != nil {
		return nil, err
	}
	oldChild, err := s.loadRootChild(oldRoot, version-1)
	if err != nil {
		return nil, err
	}
	newChild, err := s.loadRootChild(newRoot, version)
	if err != nil {
		return nil, err
	}

	oldLeaves := make(map[Hash]leafNode)
	newLeaves := make(map[Hash]leafNode)
	prefix := make([]uint8, 0, nibbles)
	if err := s.diffChildren(oldChild, newChild, &prefix, oldLeaves, newLeaves); err != nil {
		return nil, err
	}

	historyPath := s.pathOf(HistoryKey)
	var out []Change
	for path, leaf := range newLeaves {
		if path == historyPath {
			continue
		}
		if old, ok := oldLeaves[path]; ok && old.vh == leaf.vh {
			continue // relocated only; value unchanged
		}
		value, err := s.readValue(&leaf.path, &leaf.vh, leaf.valueVersion)
		if err != nil {
			return nil, err
		}
		out = append(out, Change{Path: path, Value: value})
	}
	for path := range oldLeaves {
		if path == historyPath {
			continue
		}
		if _, ok := newLeaves[path]; !ok {
			out = append(out, Change{Path: path, Deleted: true})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return string(out[i].Path[:]) < string(out[j].Path[:])
	})
	return out, nil
}

// diffChildren walks two subtrees in lockstep, collecting the leaves
// of every region whose hashes differ. Hash equality prunes the walk:
// node hashes are pure functions of logical content, so equal hashes
// mean identical subtrees whatever their record versions.
func (s *Store) diffChildren(a, b *child, prefix *[]uint8, aOut, bOut map[Hash]leafNode) error {
	if a == nil && b == nil {
		return nil
	}
	if a != nil && b != nil && a.hash == b.hash {
		return nil
	}
	na, err := s.diffLoad(a, prefix)
	if err != nil {
		return err
	}
	nb, err := s.diffLoad(b, prefix)
	if err != nil {
		return err
	}
	if na != nil && na.leaf != nil {
		aOut[na.leaf.path] = *na.leaf
	}
	if nb != nil && nb.leaf != nil {
		bOut[nb.leaf.path] = *nb.leaf
	}
	var ca, cb *internalNode
	if na != nil && na.internal != nil {
		ca = na.internal
	}
	if nb != nil && nb.internal != nil {
		cb = nb.internal
	}
	if ca == nil && cb == nil {
		return nil
	}
	for nib := 0; nib < 16; nib++ {
		var childA, childB *child
		if ca != nil {
			childA = ca.children[nib]
		}
		if cb != nil {
			childB = cb.children[nib]
		}
		*prefix = append(*prefix, uint8(nib))
		err := s.diffChildren(childA, childB, prefix, aOut, bOut)
		*prefix = (*prefix)[:len(*prefix)-1]
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) diffLoad(c *child, prefix *[]uint8) (*node, error) {
	if c == nil {
		return nil, nil
	}
	nk := nodeKey{version: c.version, prefix: append([]uint8(nil), *prefix...)}
	return s.loadNode(&nk, &c.hash)
}
