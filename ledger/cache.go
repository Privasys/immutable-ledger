// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import (
	"container/list"
	"sync"
)

// nodeCache is an LRU over node records.
//
// Node records are immutable (a nodeKey is written by exactly one
// commit and never again), so the cache needs no invalidation logic to
// stay coherent — only pruning clears it wholesale, to keep deleted
// history from lingering. Cached nodes are still hash-verified against
// the parent's expectation on every use: the cache removes backend I/O,
// never trust checks.
//
// Eviction is least-recently-used in O(1): a list in recency order with
// a map into it. A read that walks the tree touches one node per level,
// so the cache has to hold the upper levels of a large tree whole to be
// worth having, and a miss must not cost a scan of the cache.
type nodeCache struct {
	mu       sync.Mutex
	capacity int
	order    *list.List               // front: most recently used
	m        map[string]*list.Element // keyed by encoded nodeKey
}

type cacheEntry struct {
	key  string
	node *node
}

// newNodeCache builds a cache; capacity 0 disables caching entirely.
func newNodeCache(capacity int) *nodeCache {
	return &nodeCache{capacity: capacity, order: list.New(), m: make(map[string]*list.Element)}
}

func (c *nodeCache) get(nk *nodeKey) *node {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[string(nk.encode())]
	if !ok {
		return nil
	}
	c.order.MoveToFront(e)
	return e.Value.(*cacheEntry).node
}

func (c *nodeCache) put(nk *nodeKey, n *node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity == 0 {
		return
	}
	k := string(nk.encode())
	if e, ok := c.m[k]; ok {
		e.Value.(*cacheEntry).node = n
		c.order.MoveToFront(e)
		return
	}
	for len(c.m) >= c.capacity {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.m, oldest.Value.(*cacheEntry).key)
	}
	c.m[k] = c.order.PushFront(&cacheEntry{key: k, node: n})
}

// clear drops everything (pruning; capacity changes).
func (c *nodeCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.order.Init()
	c.m = make(map[string]*list.Element)
}

func (c *nodeCache) setCapacity(capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capacity = capacity
	c.order.Init()
	c.m = make(map[string]*list.Element)
}

func (c *nodeCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
