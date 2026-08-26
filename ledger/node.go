// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import "encoding/binary"

// Tree node model: versioned node keys, internal/leaf nodes, binary
// codecs and node hashing.
//
// Storage is 16-ary (one node per nibble level) but hashing is binary:
// an internal node's hash is the root of a 4-level binary subtree over
// its 16 child slots, with empty ranges standing in as the placeholder
// hash and single-leaf ranges collapsing to the leaf hash. That keeps
// I/O at log16 while proofs stay compact binary sibling lists.

// nibbles is the total nibble count of a full path (256-bit, 4 bits each).
const nibbles = 64

// nibble returns nibble i of a 32-byte path, MSB-first.
func nibble(path *Hash, i int) uint8 {
	b := path[i/2]
	if i%2 == 0 {
		return b >> 4
	}
	return b & 0x0F
}

// nodeKey is where a node lives: the version at which it was written
// plus its position (nibble prefix) in the tree. Node records are
// immutable: a commit writes new nodes under the new version and never
// mutates old ones.
type nodeKey struct {
	version uint64
	// prefix holds nibble values (each 0..=15) from the root; empty = root.
	prefix []uint8
}

func rootNodeKey(version uint64) nodeKey {
	return nodeKey{version: version, prefix: nil}
}

// encode packs the key as `version u64 BE ‖ nibble_count u8 ‖ packed nibbles`.
func (nk *nodeKey) encode() []byte {
	buf := make([]byte, 0, 9+(len(nk.prefix)+1)/2)
	buf = binary.BigEndian.AppendUint64(buf, nk.version)
	buf = append(buf, uint8(len(nk.prefix)))
	for i := 0; i < len(nk.prefix); i += 2 {
		hi := nk.prefix[i] << 4
		var lo uint8
		if i+1 < len(nk.prefix) {
			lo = nk.prefix[i+1]
		}
		buf = append(buf, hi|lo)
	}
	return buf
}

// child is a reference to a child node held inside an internal node.
type child struct {
	// version component of the child's nodeKey.
	version uint64
	// hash of the child node; verified against the loaded record.
	hash Hash
	// isLeaf records whether the child is a leaf (hashing and collapse).
	isLeaf bool
}

// internalNode holds up to 16 children, one per nibble value.
type internalNode struct {
	children [16]*child
}

// leafNode terminates the path however deep it sits. path is the full
// 256-bit key path; vh is the keyed plaintext commitment of the value.
//
// valueVersion is the commit version at which the value record was
// written: it routes reads to the versioned value record
// (`v ‖ vh ‖ value_version`). It is storage metadata, NOT part of the
// leaf hash — the hash commits to (path, vh) only, so the root stays a
// pure function of logical state. A tampered valueVersion can only
// point at a missing record (fail closed) or at a record whose content
// still has to satisfy vh (same plaintext).
type leafNode struct {
	path         Hash
	vh           Hash
	valueVersion uint64
}

// node is either an internal node or a leaf.
type node struct {
	// Exactly one of internal/leaf is non-nil.
	internal *internalNode
	leaf     *leafNode
}

const (
	nodeTagInternal = 1
	nodeTagLeaf     = 2
)

func (n *node) isLeaf() bool { return n.leaf != nil }

// encode produces the stored byte form.
//
// Leaf: `[2] path(32) vh(32) value_version u64 BE`.
// Internal: `[1] bitmap u16 LE { version u64 BE, hash(32), is_leaf u8 }*`
// with children serialized in ascending nibble order of set bits.
func (n *node) encode() []byte {
	if l := n.leaf; l != nil {
		buf := make([]byte, 0, 1+2*HashSize+8)
		buf = append(buf, nodeTagLeaf)
		buf = append(buf, l.path[:]...)
		buf = append(buf, l.vh[:]...)
		buf = binary.BigEndian.AppendUint64(buf, l.valueVersion)
		return buf
	}
	var bitmap uint16
	count := 0
	for i, c := range n.internal.children {
		if c != nil {
			bitmap |= 1 << i
			count++
		}
	}
	buf := make([]byte, 0, 3+count*(8+HashSize+1))
	buf = append(buf, nodeTagInternal)
	buf = binary.LittleEndian.AppendUint16(buf, bitmap)
	for _, c := range n.internal.children {
		if c == nil {
			continue
		}
		buf = binary.BigEndian.AppendUint64(buf, c.version)
		buf = append(buf, c.hash[:]...)
		if c.isLeaf {
			buf = append(buf, 1)
		} else {
			buf = append(buf, 0)
		}
	}
	return buf
}

// decodeNode parses a stored record. Strict: trailing bytes or
// malformed content are corruption, never ignored.
func decodeNode(data []byte) (*node, error) {
	if len(data) == 0 {
		return nil, errCorrupted("node decode: unknown tag")
	}
	switch data[0] {
	case nodeTagLeaf:
		if len(data) != 1+2*HashSize+8 {
			return nil, errCorrupted("node decode: bad leaf length")
		}
		l := &leafNode{}
		copy(l.path[:], data[1:1+HashSize])
		copy(l.vh[:], data[1+HashSize:1+2*HashSize])
		l.valueVersion = binary.BigEndian.Uint64(data[1+2*HashSize:])
		return &node{leaf: l}, nil
	case nodeTagInternal:
		if len(data) < 3 {
			return nil, errCorrupted("node decode: internal too short")
		}
		bitmap := binary.LittleEndian.Uint16(data[1:3])
		count := 0
		for i := 0; i < 16; i++ {
			if bitmap&(1<<i) != 0 {
				count++
			}
		}
		if count == 0 {
			return nil, errCorrupted("node decode: internal with no children")
		}
		const entry = 8 + HashSize + 1
		if len(data) != 3+count*entry {
			return nil, errCorrupted("node decode: bad internal length")
		}
		in := &internalNode{}
		off := 3
		for i := 0; i < 16; i++ {
			if bitmap&(1<<i) == 0 {
				continue
			}
			c := &child{version: binary.BigEndian.Uint64(data[off : off+8])}
			copy(c.hash[:], data[off+8:off+8+HashSize])
			switch data[off+8+HashSize] {
			case 0:
				c.isLeaf = false
			case 1:
				c.isLeaf = true
			default:
				return nil, errCorrupted("node decode: bad is_leaf flag")
			}
			in.children[i] = c
			off += entry
		}
		return &node{internal: in}, nil
	default:
		return nil, errCorrupted("node decode: unknown tag")
	}
}

// hash is the node's hash, as committed by its parent (or the root).
func (n *node) hash() Hash {
	if l := n.leaf; l != nil {
		return leafHash(&l.path, &l.vh)
	}
	return subtreeHash(&n.internal.children, 0, 16)
}

// subtreeHash folds a binary merkle over a range of child slots:
//
//   - empty range → placeholder
//   - single slot → that child's hash (or placeholder)
//   - range holding exactly one child which is a leaf → the leaf's hash
//     (the leaf represents the whole subtree, wherever it sits)
//   - otherwise → internalHash(left half, right half)
//
// The prover folds sibling half-ranges with this too.
func subtreeHash(children *[16]*child, start, width int) Hash {
	var only *child
	present := 0
	for i := start; i < start+width; i++ {
		if c := children[i]; c != nil {
			present++
			if present == 1 {
				only = c
			} else {
				break
			}
		}
	}
	switch {
	case present == 0:
		return placeholderHash
	case present == 1 && (width == 1 || only.isLeaf):
		return only.hash
	default:
		half := width / 2
		left := subtreeHash(children, start, half)
		right := subtreeHash(children, start+half, half)
		return internalHash(&left, &right)
	}
}
