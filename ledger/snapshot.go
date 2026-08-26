// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

// Leaf export and restore: chunked, path-ordered iteration over the
// tree at a version (backup, replication, state transfer), and the
// matching restore path that rebuilds a store from exported leaves.
//
// Exports carry (path, plaintext) pairs: paths are keyed hashes, so the
// logical keys are neither recoverable nor needed — a restored tree is
// keyed by path exactly like the original, and produces the same root
// when it holds the same logical data under the same commitment key.

// Leaf is one exported (path, value) pair.
type Leaf struct {
	Path  Hash
	Value []byte
}

// SnapshotLeaves collects up to max leaves of the tree at version, in
// path order, starting strictly after startAfter (nil = from the
// start). Each value is read, decrypted and commitment-verified
// (fail-closed). done is false when more leaves remain.
//
// Chunked iteration re-descends only the resume path (subtrees entirely
// at or below startAfter are skipped by nibble comparison), so a full
// scan costs O(n) total.
func (s *Store) SnapshotLeaves(version uint64, startAfter *Hash, max int) (leaves []Leaf, done bool, err error) {
	root, err := s.loadRootRecord(version)
	if err != nil {
		return nil, false, err
	}
	rootChild, err := s.loadRootChild(root, version)
	if err != nil {
		return nil, false, err
	}
	if rootChild == nil {
		return nil, true, nil
	}
	prefix := make([]uint8, 0, nibbles)
	done, err = s.collectLeaves(rootChild, &prefix, startAfter, max, &leaves)
	return leaves, done, err
}

// collectLeaves runs a DFS in nibble order; it reports true when the
// subtree was exhausted.
func (s *Store) collectLeaves(c *child, prefix *[]uint8, startAfter *Hash, max int, out *[]Leaf) (bool, error) {
	if len(*out) >= max {
		return false, nil
	}
	nk := nodeKey{version: c.version, prefix: append([]uint8(nil), *prefix...)}
	n, err := s.loadNode(&nk, &c.hash)
	if err != nil {
		return false, err
	}
	if leaf := n.leaf; leaf != nil {
		if startAfter != nil && !pathGreater(&leaf.path, startAfter) {
			return true, nil // already streamed
		}
		value, err := s.readValue(&leaf.path, &leaf.vh, leaf.valueVersion)
		if err != nil {
			return false, err
		}
		*out = append(*out, Leaf{Path: leaf.path, Value: value})
		return true, nil
	}
	depth := len(*prefix)
	resumeNib := -1
	if startAfter != nil {
		resumeNib = int(nibble(startAfter, depth))
	}
	for nib := 0; nib < 16; nib++ {
		// Children below the resume nibble hold only paths <=
		// startAfter: skip whole subtrees.
		if resumeNib >= 0 && nib < resumeNib {
			continue
		}
		cc := n.internal.children[nib]
		if cc == nil {
			continue
		}
		// Only the resume child keeps the startAfter bound.
		var sa *Hash
		if resumeNib >= 0 && nib == resumeNib {
			sa = startAfter
		}
		*prefix = append(*prefix, uint8(nib))
		exhausted, err := s.collectLeaves(cc, prefix, sa, max, out)
		*prefix = (*prefix)[:len(*prefix)-1]
		if err != nil {
			return false, err
		}
		if !exhausted {
			return false, nil
		}
	}
	return true, nil
}

func pathGreater(a, b *Hash) bool {
	for i := 0; i < HashSize; i++ {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// PinVersion pins a version against pruning (a snapshot is being served
// from it). Pass nil to release the pin.
func (s *Store) PinVersion(version *uint64) { s.snapshotPin = version }

// RestoreLeaves inserts plaintext values addressed by PATH (not logical
// key) — the snapshot-restore path. One atomic commit; returns the new
// (root, version).
func (s *Store) RestoreLeaves(leaves []Leaf) (Hash, uint64, error) {
	deduped := make(map[Hash][]byte, len(leaves))
	for i := range leaves {
		deduped[leaves[i].Path] = leaves[i].Value
	}
	if len(deduped) == 0 {
		return s.root, s.version, nil
	}
	acc := &commitAcc{
		newVersion: s.version + 1,
		pending:    make(map[string]pendingNode),
		insertCTs:  make(map[Hash][]byte),
		values:     make(map[Hash][]byte),
	}
	updates := make([]update, 0, len(deduped))
	for path, pt := range deduped {
		vh := s.vhOf(&path, pt)
		ct, err := s.sealValue(&path, pt)
		if err != nil {
			return Hash{}, 0, err
		}
		acc.insertCTs[path] = ct
		vhCopy := vh
		updates = append(updates, update{path: path, vh: &vhCopy})
	}
	sortUpdates(updates)
	prefix := make([]uint8, 0, nibbles)
	newRootChild, err := s.applySubtree(acc, s.rootChild, &prefix, updates)
	if err != nil {
		return Hash{}, 0, err
	}
	if childEq(newRootChild, s.rootChild) {
		return s.root, s.version, nil
	}
	newRoot := placeholderHash
	if newRootChild != nil {
		newRoot = newRootChild.hash
	}
	return s.commitAcc(acc, newRootChild, newRoot)
}

// StampVersion is the snapshot-restore epilogue: stamp the store at
// version (>= the current build version), writing the root record and
// checkpoint for it, and duplicating the root NODE record at the
// stamped version so a later Open(root, version) resolves. Interior
// node records keep their (smaller) build versions, which child
// references tolerate (each carries its own version).
func (s *Store) StampVersion(version uint64) error {
	if version < s.version {
		return errInvalidf("cannot stamp version %d below current %d", version, s.version)
	}
	ckpt, err := s.sealCheckpoint(&s.root, version)
	if err != nil {
		return err
	}
	batch := []BatchOp{
		{Key: rootRecordKey(version), Value: s.root[:]},
		{Key: checkpointRecordKey(), Value: ckpt},
	}
	if c := s.rootChild; c != nil {
		nk := nodeKey{version: c.version}
		n, err := s.loadNode(&nk, &c.hash)
		if err != nil {
			return err
		}
		stamped := nodeKey{version: version}
		batch = append(batch, BatchOp{Key: nodeRecordKey(&stamped), Value: n.encode()})
		s.rootChild = &child{version: version, hash: c.hash, isLeaf: c.isLeaf}
	}
	if err := s.backend.WriteBatch(batch); err != nil {
		return errBackend(err)
	}
	s.version = version
	return nil
}
