// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"bytes"
	"sort"
	"sync"
)

// The store talks to storage through Backend: point get, atomic write
// batch, and an ascending range scan. Implement it over whatever the
// deployment persists to (an embedded KV store on the encrypted data
// partition, typically).
//
// Node records are intentionally plaintext to the backend (they hold
// only hashes, versions and nibble offsets): integrity comes from hash
// verification against the in-memory root on every read, and value
// bytes are AES-256-GCM ciphertext.

// BatchOp is one operation of an atomic write batch. A nil Value with
// Delete=false stores an empty record; set Delete for removals.
type BatchOp struct {
	Key    []byte
	Value  []byte
	Delete bool
}

// KV is one scanned record.
type KV struct {
	Key   []byte
	Value []byte
}

// Backend is the storage surface the tree requires.
type Backend interface {
	// Get returns the record at key, or (nil, false, nil) when absent.
	Get(key []byte) (value []byte, ok bool, err error)
	// WriteBatch applies all ops atomically: either every op lands or
	// none does. Atomicity is what makes commits crash-safe.
	WriteBatch(ops []BatchOp) error
	// Scan returns records in [start, end) in ascending key order, at
	// most limit entries (0 = unlimited). An empty end is unbounded.
	Scan(start, end []byte, limit uint32) ([]KV, error)
}

// MemBackend is an in-memory Backend for tests: it counts reads
// (I/O-complexity assertions) and can tamper with stored records
// (fail-closed assertions).
type MemBackend struct {
	mu    sync.Mutex
	m     map[string][]byte
	reads int
}

// NewMemBackend returns an empty in-memory backend.
func NewMemBackend() *MemBackend {
	return &MemBackend{m: make(map[string][]byte)}
}

// Reads returns the number of point reads served since construction or
// the last ResetReads.
func (b *MemBackend) Reads() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reads
}

// ResetReads zeroes the read counter.
func (b *MemBackend) ResetReads() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads = 0
}

// Len returns the number of records stored.
func (b *MemBackend) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.m)
}

// Keys returns all record keys (for tamper sweeps).
func (b *MemBackend) Keys() [][]byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([][]byte, 0, len(b.m))
	for k := range b.m {
		out = append(out, []byte(k))
	}
	sort.Slice(out, func(i, j int) bool { return bytes.Compare(out[i], out[j]) < 0 })
	return out
}

// Tamper flips one bit of the record at key. Returns false if absent.
func (b *MemBackend) Tamper(key []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	v, ok := b.m[string(key)]
	if !ok || len(v) == 0 {
		return false
	}
	c := append([]byte(nil), v...)
	c[0] ^= 0x01
	b.m[string(key)] = c
	return true
}

// Remove deletes a record outright (storage "loses" data).
func (b *MemBackend) Remove(key []byte) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.m[string(key)]
	delete(b.m, string(key))
	return ok
}

// Get implements Backend.
func (b *MemBackend) Get(key []byte) ([]byte, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reads++
	v, ok := b.m[string(key)]
	if !ok {
		return nil, false, nil
	}
	return append([]byte(nil), v...), true, nil
}

// WriteBatch implements Backend.
func (b *MemBackend) WriteBatch(ops []BatchOp) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, op := range ops {
		if op.Delete {
			delete(b.m, string(op.Key))
		} else {
			b.m[string(op.Key)] = append([]byte(nil), op.Value...)
		}
	}
	return nil
}

// Scan implements Backend.
func (b *MemBackend) Scan(start, end []byte, limit uint32) ([]KV, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	keys := make([]string, 0, len(b.m))
	for k := range b.m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	max := int(limit)
	if limit == 0 {
		max = len(keys)
	}
	out := make([]KV, 0, max)
	for _, k := range keys {
		kb := []byte(k)
		if bytes.Compare(kb, start) < 0 {
			continue
		}
		if len(end) > 0 && bytes.Compare(kb, end) >= 0 {
			break
		}
		out = append(out, KV{Key: kb, Value: append([]byte(nil), b.m[k]...)})
		if len(out) >= max {
			break
		}
	}
	return out, nil
}
