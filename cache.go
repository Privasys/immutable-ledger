// Copyright (c) Privasys. All rights reserved.
// Licensed under the GNU Affero General Public License v3.0. See LICENSE file for details.

package ledger

import "sync"

// nodeCache is an LRU over node records.
//
// Node records are immutable (a nodeKey is written by exactly one
// commit and never again), so the cache needs no invalidation logic to
// stay coherent — only pruning clears it wholesale, to keep deleted
// history from lingering. Cached nodes are still hash-verified against
// the parent's expectation on every use: the cache removes backend I/O,
// never trust checks.
//
// Eviction is least-recently-used via a monotonic access tick with an
// O(capacity) scan on eviction — capacities are ~10³ and evictions are
// per cache-miss, so the scan is noise next to a storage round trip.
type nodeCache struct {
	mu       sync.Mutex
	capacity int
	tick     uint64
	m        map[string]cacheEntry // keyed by encoded nodeKey
}

type cacheEntry struct {
	node     *node
	lastUsed uint64
}

// newNodeCache builds a cache; capacity 0 disables caching entirely.
func newNodeCache(capacity int) *nodeCache {
	return &nodeCache{capacity: capacity, m: make(map[string]cacheEntry)}
}

func (c *nodeCache) get(nk *nodeKey) *node {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.tick++
	k := string(nk.encode())
	e, ok := c.m[k]
	if !ok {
		return nil
	}
	e.lastUsed = c.tick
	c.m[k] = e
	return e.node
}

func (c *nodeCache) put(nk *nodeKey, n *node) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.capacity == 0 {
		return
	}
	k := string(nk.encode())
	if _, exists := c.m[k]; !exists && len(c.m) >= c.capacity {
		var oldestKey string
		var oldestTick uint64
		first := true
		for key, e := range c.m {
			if first || e.lastUsed < oldestTick {
				oldestKey, oldestTick, first = key, e.lastUsed, false
			}
		}
		delete(c.m, oldestKey)
	}
	c.tick++
	c.m[k] = cacheEntry{node: n, lastUsed: c.tick}
}

// clear drops everything (pruning; capacity changes).
func (c *nodeCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m = make(map[string]cacheEntry)
}

func (c *nodeCache) setCapacity(capacity int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.capacity = capacity
	c.m = make(map[string]cacheEntry)
}

func (c *nodeCache) len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.m)
}
