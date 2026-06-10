// SPDX-License-Identifier: Apache-2.0

package object

import (
	"sort"
	"sync"
	"time"
)

// --- per-object ObjectInfo cache -------------------------------------------

const (
	objCacheTTL = 5 * time.Second
	objCacheMax = 8192 // max entries; simple map, lazy eviction via TTL
)

// objectCache caches ObjectInfo results keyed by "bucket\x00key".  It is
// a write-through cache: putObject populates it, deleteObject removes it.
// On a STAT/HEAD cache miss the caller reads from drives and populates the
// cache; subsequent calls for the same hot object skip the 4-drive read.
// Eviction is lazy (checked on read) and the map is unbounded up to objCacheMax;
// beyond that we simply stop caching new entries until the next GC sweep clears
// enough TTL-expired entries.
type objectCache struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]objCacheEntry
}

type objCacheEntry struct {
	oi ObjectInfo
	at time.Time
}

func newObjectCache() *objectCache {
	return &objectCache{now: time.Now, m: make(map[string]objCacheEntry, 256)}
}

func (c *objectCache) key(bucket, object string) string { return bucket + "\x00" + object }

func (c *objectCache) get(bucket, object string) (ObjectInfo, bool) {
	c.mu.Lock()
	e, ok := c.m[c.key(bucket, object)]
	c.mu.Unlock()
	if !ok {
		return ObjectInfo{}, false
	}
	if c.now().Sub(e.at) > objCacheTTL {
		c.del(bucket, object)
		return ObjectInfo{}, false
	}
	return e.oi, true
}

func (c *objectCache) set(bucket, object string, oi ObjectInfo) {
	c.mu.Lock()
	if len(c.m) < objCacheMax {
		c.m[c.key(bucket, object)] = objCacheEntry{oi: oi, at: c.now()}
	}
	c.mu.Unlock()
}

func (c *objectCache) del(bucket, object string) {
	c.mu.Lock()
	delete(c.m, c.key(bucket, object))
	c.mu.Unlock()
}

// invalidateBucket removes all entries for a bucket. Called on bucket delete.
func (c *objectCache) invalidateBucket(bucket string) {
	prefix := bucket + "\x00"
	c.mu.Lock()
	for k := range c.m {
		if len(k) > len(prefix) && k[:len(prefix)] == prefix {
			delete(c.m, k)
		}
	}
	c.mu.Unlock()
}

// Metacache bounds (spec doc 07.5). The TTL caps how long a cached namespace walk
// is served before it is re-walked; the bucket cap bounds memory by evicting the
// oldest walk when the cache is full.
const (
	metacacheTTL        = 10 * time.Second
	metacacheMaxBuckets = 256
)

// metacacheEntry is one bucket's cached namespace walk: the sorted, de-duplicated
// union of object keys and the time it was populated.
type metacacheEntry struct {
	keys     []string
	cachedAt time.Time
}

// metacache accelerates listing by caching the result of a bucket's namespace
// walk — the expensive, index-free tree descent that every ListObjectsV2 and
// ListObjectVersions would otherwise repeat on every page (spec doc 07.5).
//
// It caches only the set of keys that exist, never per-key or per-version
// metadata: that is always read fresh from obj.meta, so the cache is an
// accelerator and never a source of truth. A write inserts its key (keeping a
// warm cache correct without a re-walk); a delete invalidates the bucket so the
// next list re-reads the tree. Entries are age- and count-bounded.
type metacache struct {
	mu  sync.Mutex
	now func() time.Time // injectable clock; time.Now in production
	m   map[string]metacacheEntry
}

// newMetacache builds an empty metacache with a real clock.
func newMetacache() *metacache {
	return &metacache{now: time.Now, m: map[string]metacacheEntry{}}
}

// keys returns the bucket's sorted key union, serving a fresh-enough cached walk
// or populating the cache by calling walk on a miss or after the TTL. walk runs
// outside the lock so a slow tree descent does not block other buckets.
func (c *metacache) keys(bucket string, walk func() ([]string, error)) ([]string, error) {
	c.mu.Lock()
	if e, ok := c.m[bucket]; ok && c.now().Sub(e.cachedAt) < metacacheTTL {
		keys := e.keys
		c.mu.Unlock()
		return keys, nil
	}
	c.mu.Unlock()

	keys, err := walk()
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.evictIfFull()
	c.m[bucket] = metacacheEntry{keys: keys, cachedAt: c.now()}
	c.mu.Unlock()
	return keys, nil
}

// add records that key exists so a warm cache stays correct across a write
// without a re-walk. It is a no-op when the bucket is not cached (the next list
// populates it from disk). The slice is copied on write so a concurrent reader
// holding the previous slice is never mutated underneath it.
func (c *metacache) add(bucket, key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[bucket]
	if !ok {
		return
	}
	i := sort.SearchStrings(e.keys, key)
	if i < len(e.keys) && e.keys[i] == key {
		return // already present
	}
	next := make([]string, 0, len(e.keys)+1)
	next = append(next, e.keys[:i]...)
	next = append(next, key)
	next = append(next, e.keys[i:]...)
	c.m[bucket] = metacacheEntry{keys: next, cachedAt: e.cachedAt}
}

// invalidate drops a bucket's cached walk so the next list re-reads the tree.
// Deletes use it because a removed key (or a versioned delete that changes which
// version is current) is simpler to reflect by re-walking than to patch in place.
func (c *metacache) invalidate(bucket string) {
	c.mu.Lock()
	delete(c.m, bucket)
	c.mu.Unlock()
}

// evictIfFull drops the oldest entry when the cache is at its bucket cap. The
// caller must hold the lock.
func (c *metacache) evictIfFull() {
	if len(c.m) < metacacheMaxBuckets {
		return
	}
	var oldest string
	var oldestAt time.Time
	for b, e := range c.m {
		if oldest == "" || e.cachedAt.Before(oldestAt) {
			oldest, oldestAt = b, e.cachedAt
		}
	}
	delete(c.m, oldest)
}
