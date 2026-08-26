// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import "encoding/binary"

// Inclusion and absence proofs, with a pure verifier.
//
// # Shape
//
// Storage is 16-ary but hashing is binary, so every proof is a plain
// binary sparse-Merkle proof over the 256-bit path:
//
//   - Leaf: the leaf found at the end of the descent, if any. Its path
//     equal to the proven path ⇒ inclusion; different ⇒ absence
//     evidence (the position the path would occupy is covered by
//     another leaf). Absent ⇒ the descent ended on an empty subtree.
//   - Siblings: sibling hashes bottom-up. Sibling i (0 = deepest) pairs
//     at binary depth len-1-i, and the proven path's bit at that depth
//     picks the fold side.
//
// # Verification statement
//
// Verify recomputes the root and returns what the proof proves about
// path: present (with the value commitment vh) or absent. Callers
// holding the commitment key compare vh against
// HMAC-SHA256(ck, "v" ‖ path ‖ plaintext) — see Store.VerifyValue.
// Without ck, statements are about opaque (path, vh) pairs by design.
//
// The verifier is pure (no store, no backend): it runs anywhere for
// client-side verification, and the wire format is identical to the
// Rust implementation's.

// maxDepth is the maximum binary depth of the tree (256-bit paths).
const maxDepth = 256

// ProofLeaf is the terminal leaf (path, vh) found by a proof's descent.
type ProofLeaf struct {
	Path Hash
	Vh   Hash
}

// Proof proves presence or absence of one path at one root.
type Proof struct {
	// Leaf is the terminal leaf found by the descent, if any.
	Leaf *ProofLeaf
	// Siblings holds sibling hashes, bottom-up (deepest first).
	Siblings []Hash
}

// Verified is what a valid proof establishes about the proven path.
type Verified struct {
	// Present reports whether the path holds a value at the root.
	Present bool
	// Vh is the value commitment when Present.
	Vh Hash
}

// bitAt returns bit i (MSB-first) of a 32-byte path.
func bitAt(path *Hash, i int) bool {
	return path[i/8]&(0x80>>(i%8)) != 0
}

// commonPrefixBits counts the leading bits shared by two paths.
func commonPrefixBits(a, b *Hash) int {
	for i := 0; i < HashSize; i++ {
		x := a[i] ^ b[i]
		if x != 0 {
			n := 0
			for x&0x80 == 0 {
				x <<= 1
				n++
			}
			return i*8 + n
		}
	}
	return maxDepth
}

// Verify checks proof for path against root.
//
// It returns the established statement, or an error if the proof does
// not recompute root (or is malformed). Pure function.
func Verify(root, path *Hash, proof *Proof) (Verified, error) {
	n := len(proof.Siblings)
	if n > maxDepth {
		return Verified{}, errInvalid("proof too deep")
	}

	var acc Hash
	var statement Verified
	if l := proof.Leaf; l != nil {
		if l.Path == *path {
			acc = leafHash(&l.Path, &l.Vh)
			statement = Verified{Present: true, Vh: l.Vh}
		} else {
			// Absence via a divergent leaf: it must actually cover the
			// position the proven path would occupy, i.e. agree with
			// the path on every bit the fold consumes.
			if commonPrefixBits(&l.Path, path) < n {
				return Verified{}, errInvalid("divergent leaf does not cover the proven path")
			}
			acc = leafHash(&l.Path, &l.Vh)
		}
	} else {
		acc = placeholderHash
	}

	for i := range proof.Siblings {
		depth := n - 1 - i
		sib := proof.Siblings[i]
		if bitAt(path, depth) {
			acc = internalHash(&sib, &acc)
		} else {
			acc = internalHash(&acc, &sib)
		}
	}

	if acc != *root {
		return Verified{}, errInvalid("proof does not match root")
	}
	return statement, nil
}

// Encode packs the proof as
// `[u8 flags(bit0 = has_leaf)] (path 32 ‖ vh 32)? [u16 LE count] siblings*32`.
func (p *Proof) Encode() []byte {
	buf := make([]byte, 0, 3+2*HashSize+len(p.Siblings)*HashSize)
	if l := p.Leaf; l != nil {
		buf = append(buf, 1)
		buf = append(buf, l.Path[:]...)
		buf = append(buf, l.Vh[:]...)
	} else {
		buf = append(buf, 0)
	}
	buf = binary.LittleEndian.AppendUint16(buf, uint16(len(p.Siblings)))
	for i := range p.Siblings {
		buf = append(buf, p.Siblings[i][:]...)
	}
	return buf
}

// DecodeProof is the strict decoder of Encode's format.
func DecodeProof(data []byte) (*Proof, error) {
	malformed := errInvalid("malformed proof")
	if len(data) == 0 {
		return nil, malformed
	}
	var leaf *ProofLeaf
	off := 1
	switch data[0] {
	case 0:
	case 1:
		if len(data) < 1+2*HashSize {
			return nil, malformed
		}
		leaf = &ProofLeaf{}
		copy(leaf.Path[:], data[1:1+HashSize])
		copy(leaf.Vh[:], data[1+HashSize:1+2*HashSize])
		off = 1 + 2*HashSize
	default:
		return nil, malformed
	}
	if len(data) < off+2 {
		return nil, malformed
	}
	count := int(binary.LittleEndian.Uint16(data[off : off+2]))
	off += 2
	if count > maxDepth || len(data) != off+count*HashSize {
		return nil, malformed
	}
	siblings := make([]Hash, count)
	for i := 0; i < count; i++ {
		copy(siblings[i][:], data[off:off+HashSize])
		off += HashSize
	}
	return &Proof{Leaf: leaf, Siblings: siblings}, nil
}

// -- store-side proving ---------------------------------------------------

// Prove proves presence or absence of key at the current root.
func (s *Store) Prove(key []byte) (*Proof, error) {
	path := s.pathOf(key)
	return s.provePath(s.rootChild, &path)
}

// ProveAt proves presence or absence of key at a historical version
// (same freshness caveat as GetAt).
func (s *Store) ProveAt(version uint64, key []byte) (*Proof, error) {
	if version > s.version {
		return nil, errInvalidf("version %d is in the future (current %d)", version, s.version)
	}
	root, err := s.loadRootRecord(version)
	if err != nil {
		return nil, err
	}
	rootChild, err := s.loadRootChild(root, version)
	if err != nil {
		return nil, err
	}
	path := s.pathOf(key)
	return s.provePath(rootChild, &path)
}

// VerifyValue checks a proof claiming key = value against root.
// (false, nil) = the proof is valid but proves something else
// (absence, or a different value).
func (s *Store) VerifyValue(root *Hash, key, value []byte, proof *Proof) (bool, error) {
	path := s.pathOf(key)
	v, err := Verify(root, &path, proof)
	if err != nil {
		return false, err
	}
	return v.Present && v.Vh == s.vhOf(&path, value), nil
}

// VerifyAbsent checks a proof claiming key is absent at root.
func (s *Store) VerifyAbsent(root *Hash, key []byte, proof *Proof) (bool, error) {
	path := s.pathOf(key)
	v, err := Verify(root, &path, proof)
	if err != nil {
		return false, err
	}
	return !v.Present, nil
}

// provePath walks the tree for path, collecting binary sibling hashes.
func (s *Store) provePath(rootChild *child, path *Hash) (*Proof, error) {
	var siblings []Hash // collected top-down
	c := rootChild
	if c == nil {
		return &Proof{}, nil
	}
	prefix := make([]uint8, 0, nibbles)

	var terminal *leafNode
walk:
	for {
		nk := nodeKey{version: c.version, prefix: prefix}
		n, err := s.loadNode(&nk, &c.hash)
		if err != nil {
			return nil, err
		}
		if n.isLeaf() != c.isLeaf {
			return nil, errCorruptedf("node %v kind mismatch", &nk)
		}
		if n.leaf != nil {
			// Terminal: the resident leaf (inclusion if paths match,
			// otherwise absence evidence).
			terminal = n.leaf
			break walk
		}
		internal := n.internal

		// Binary descent inside this 16-slot node, mirroring
		// subtreeHash: collect the off-path half at every level.
		slot := int(nibble(path, len(prefix)))
		start, width := 0, 16
		for {
			var present []int
			for i := start; i < start+width; i++ {
				if internal.children[i] != nil {
					present = append(present, i)
				}
			}
			switch len(present) {
			case 0:
				terminal = nil // empty range: absence
				break walk
			case 1:
				idx := present[0]
				cc := internal.children[idx]
				if cc.isLeaf {
					// The range collapses to this leaf; it covers the
					// proven path's position here (inclusion if it
					// sits at slot with equal path).
					leafPrefix := append(append([]uint8(nil), prefix...), uint8(idx))
					lk := nodeKey{version: cc.version, prefix: leafPrefix}
					ln, err := s.loadNode(&lk, &cc.hash)
					if err != nil {
						return nil, err
					}
					if ln.leaf == nil {
						return nil, errCorruptedf("node %v kind mismatch", &lk)
					}
					terminal = ln.leaf
					break walk
				}
				if width == 1 {
					// Singleton range holding an internal child:
					// descend one nibble deeper.
					prefix = append(prefix, uint8(slot))
					c = cc
					continue walk
				}
			}
			// Halve towards the slot, collecting the other half.
			half := width / 2
			var ours, sib int
			if slot < start+half {
				ours, sib = start, start+half
			} else {
				ours, sib = start+half, start
			}
			siblings = append(siblings, subtreeHash(&internal.children, sib, half))
			start = ours
			width = half
		}
	}

	// Reverse: bottom-up, as the verifier folds.
	for i, j := 0, len(siblings)-1; i < j; i, j = i+1, j-1 {
		siblings[i], siblings[j] = siblings[j], siblings[i]
	}
	p := &Proof{Siblings: siblings}
	if terminal != nil {
		p.Leaf = &ProofLeaf{Path: terminal.path, Vh: terminal.vh}
	}
	return p, nil
}
