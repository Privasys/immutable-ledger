// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import "testing"

// The cache keeps what was used last and drops what was used least
// recently, at its capacity, whatever the order of puts.
func TestNodeCacheEvictsTheLeastRecentlyUsed(t *testing.T) {
	c := newNodeCache(3)
	key := func(v uint64) *nodeKey { return &nodeKey{version: v, prefix: []uint8{1, 2}} }
	nodes := map[uint64]*node{}
	for v := uint64(1); v <= 3; v++ {
		nodes[v] = &node{leaf: &leafNode{}}
		c.put(key(v), nodes[v])
	}
	if c.get(key(1)) != nodes[1] { // 1 is now the most recent
		t.Fatal("1 not cached")
	}
	nodes[4] = &node{leaf: &leafNode{}}
	c.put(key(4), nodes[4]) // evicts 2, the least recently used
	if c.get(key(2)) != nil {
		t.Error("2 was kept")
	}
	for _, v := range []uint64{1, 3, 4} {
		if c.get(key(v)) != nodes[v] {
			t.Errorf("%d was dropped", v)
		}
	}
	if c.len() != 3 {
		t.Errorf("holds %d, capacity 3", c.len())
	}
	c.put(key(3), nodes[3]) // a repeat put refreshes, never grows
	if c.len() != 3 {
		t.Errorf("a repeat put grew the cache to %d", c.len())
	}
	c.setCapacity(0)
	c.put(key(1), nodes[1])
	if c.get(key(1)) != nil {
		t.Error("a disabled cache kept a node")
	}
}

// A full cache takes a put in constant time: a miss does not scan it.
func BenchmarkNodeCachePutWhenFull(b *testing.B) {
	c := newNodeCache(65536)
	n := &node{leaf: &leafNode{}}
	for i := 0; i < 65536; i++ {
		c.put(&nodeKey{version: uint64(i)}, n)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.put(&nodeKey{version: uint64(65536 + i)}, n)
	}
}
