// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"bytes"
	"sort"
)

// Transaction forks: run business logic against a virtual copy of the
// store, then seal the result as (rootBefore, writeSet, rootAfter).
//
// A fork is a read-through overlay pinned at the store's current
// (root, version). Reads consult the buffered write-set first and fall
// through to the store; writes only touch the buffer. Nothing is
// persisted until the sealed write-set is applied via Store.PutBatch —
// which, from the same state, provably produces the previewed root,
// because the tree math is pure.
//
// The store must not be committed to while a fork is open (single
// writer); Seal fails closed if it moved.

// Fork is a pending, uncommitted transaction over a Store.
type Fork struct {
	store         *Store
	rootBefore    Hash
	versionBefore uint64
	// overlay is the buffered write-set: logical key → value (put) or
	// nil-marker (delete).
	overlay map[string]*[]byte
}

// SealedFork is the state transition a transaction proposes.
type SealedFork struct {
	RootBefore    Hash
	VersionBefore uint64
	RootAfter     Hash
	VersionAfter  uint64
	// Ops is the deterministic write-set (key-ordered).
	Ops []Op
}

// NewFork forks the store at its current (root, version).
func NewFork(store *Store) *Fork {
	root, version := store.Root()
	return &Fork{
		store:         store,
		rootBefore:    root,
		versionBefore: version,
		overlay:       make(map[string]*[]byte),
	}
}

// RootBefore returns the state this fork is based on.
func (f *Fork) RootBefore() (Hash, uint64) { return f.rootBefore, f.versionBefore }

// Get reads through the overlay, then the underlying store. ok reports
// whether the key is present.
func (f *Fork) Get(key []byte) (value []byte, ok bool, err error) {
	if pending, buffered := f.overlay[string(key)]; buffered {
		if pending == nil {
			return nil, false, nil
		}
		return append([]byte(nil), *pending...), true, nil
	}
	return f.store.Get(key)
}

// Put buffers an insert/overwrite.
func (f *Fork) Put(key, value []byte) {
	v := append([]byte(nil), value...)
	f.overlay[string(key)] = &v
}

// Delete buffers a delete.
func (f *Fork) Delete(key []byte) {
	f.overlay[string(key)] = nil
}

// PendingOps returns the number of buffered operations.
func (f *Fork) PendingOps() int { return len(f.overlay) }

// Seal computes (rootAfter, versionAfter) without committing, and hands
// back the deterministic write-set. Fails closed if the store moved
// underneath the fork.
func (f *Fork) Seal() (*SealedFork, error) {
	rootNow, versionNow := f.store.Root()
	if rootNow != f.rootBefore || versionNow != f.versionBefore {
		return nil, errInvalidf(
			"store moved under the fork (version %d != %d)", versionNow, f.versionBefore)
	}
	keys := make([]string, 0, len(f.overlay))
	for k := range f.overlay {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ops := make([]Op, 0, len(keys))
	for _, k := range keys {
		if v := f.overlay[k]; v != nil {
			ops = append(ops, Put([]byte(k), *v))
		} else {
			ops = append(ops, Del([]byte(k)))
		}
	}
	rootAfter, versionAfter, err := f.store.PreviewBatch(ops)
	if err != nil {
		return nil, err
	}
	return &SealedFork{
		RootBefore:    f.rootBefore,
		VersionBefore: f.versionBefore,
		RootAfter:     rootAfter,
		VersionAfter:  versionAfter,
		Ops:           ops,
	}, nil
}

// IsNoop reports whether this transaction is a no-op (empty or
// ineffective write-set).
func (sf *SealedFork) IsNoop() bool { return sf.RootAfter == sf.RootBefore }

// sortUpdates orders updates by path bytes (ascending).
func sortUpdates(updates []update) {
	sort.Slice(updates, func(i, j int) bool {
		return bytes.Compare(updates[i].path[:], updates[j].path[:]) < 0
	})
}
