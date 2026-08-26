// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

// Package ledger is an authenticated key-value store: a versioned
// sparse Merkle tree (16-ary storage, binary hashing) over a pluggable
// KV backend.
//
// One 32-byte root attests the entire logical data state. The root is
// encryption-independent: it commits to keyed plaintext hashes
// (HMAC-SHA-256 under a commitment key ck), never to the bytes at
// rest. At-rest confidentiality is the volume's job by default (a LUKS
// data partition inside a confidential VM, typically); an optional
// per-machine storage key (WithStorageKey) adds AES-256-GCM value
// encryption as a second layer. Replicas sharing ck compare state as
// (version, root) whatever each one does at rest.
//
// This package is the Go port of the enclave-os-mini `enclave-os-merkle`
// crate. The commitment scheme, node hashing, record encodings and
// proof format are byte-identical: a Go store and a Rust store sharing
// ck produce the same root for the same logical data, and proofs
// verify across implementations.
//
// Reads fail closed: any record that does not verify against the
// in-memory root (node hashes, GCM tags, value commitments) is an
// error, never data.
package ledger

import "crypto/sha256"

// HashSize is the size of every hash in the tree.
const HashSize = 32

// Hash is a 32-byte SHA-256 output.
type Hash = [HashSize]byte

// Domain-separation tags: every node hash is SHA-256(tag ‖ payload)
// with a distinct tag per node kind, so a leaf can never be confused
// with an internal node or a placeholder, whatever its byte content.
var (
	tagLeaf        = []byte("enclave-os-merkle:leaf:v1")
	tagInternal    = []byte("enclave-os-merkle:node:v1")
	tagPlaceholder = []byte("ENCLAVE_OS_MERKLE_PLACEHOLDER")
)

func sha256Parts(parts ...[]byte) Hash {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	var out Hash
	copy(out[:], h.Sum(nil))
	return out
}

var placeholderHash = sha256Parts(tagPlaceholder)

// Placeholder is the hash standing in for any empty subtree, at every
// height. It is also the root of an empty store.
func Placeholder() Hash { return placeholderHash }

// leafHash is SHA-256(tag ‖ path ‖ vh).
func leafHash(path, vh *Hash) Hash {
	return sha256Parts(tagLeaf, path[:], vh[:])
}

// internalHash is one binary step inside an internal node:
// SHA-256(tag ‖ left ‖ right).
func internalHash(left, right *Hash) Hash {
	return sha256Parts(tagInternal, left[:], right[:])
}
