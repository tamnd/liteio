// SPDX-License-Identifier: Apache-2.0

package object

import (
	"sort"
	"strings"
	"sync"

	"github.com/tamnd/liteio/object/index"
)

// inmemBucket holds two parallel sorted slices: keys and ois. They are kept
// in sync so keys[i] corresponds to ois[i]. Parallel arrays (vs map) give
// sequential memory access during the key scan in listObjects — no hash table
// cache misses even for 10 K-entry buckets.
//
// Reads use a read lock (concurrent LISTs don't block each other).
// Writes hold a write lock only long enough to insert/delete one entry.
type inmemBucket struct {
	mu   sync.RWMutex
	keys []string     // sorted
	ois  []ObjectInfo // parallel to keys
}

func newInmemBucket() *inmemBucket { return &inmemBucket{} }

func (b *inmemBucket) put(key string, oi ObjectInfo) {
	b.mu.Lock()
	i := sort.SearchStrings(b.keys, key)
	if i < len(b.keys) && b.keys[i] == key {
		// Update in place.
		b.ois[i] = oi
	} else {
		// Insert at position i in both slices.
		b.keys = append(b.keys, "")
		copy(b.keys[i+1:], b.keys[i:])
		b.keys[i] = key
		b.ois = append(b.ois, ObjectInfo{})
		copy(b.ois[i+1:], b.ois[i:])
		b.ois[i] = oi
	}
	b.mu.Unlock()
}

func (b *inmemBucket) del(key string) {
	b.mu.Lock()
	i := sort.SearchStrings(b.keys, key)
	if i < len(b.keys) && b.keys[i] == key {
		b.keys = append(b.keys[:i], b.keys[i+1:]...)
		b.ois = append(b.ois[:i], b.ois[i+1:]...)
	}
	b.mu.Unlock()
}

// allEntries returns a copy of all keys and ObjectInfo values in sorted order.
func (b *inmemBucket) allEntries() ([]string, []ObjectInfo) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if len(b.keys) == 0 {
		return nil, nil
	}
	keys := make([]string, len(b.keys))
	ois := make([]ObjectInfo, len(b.ois))
	copy(keys, b.keys)
	copy(ois, b.ois)
	return keys, ois
}

// listObjects applies S3 ListObjectsV2 filtering (prefix, continuation token,
// start-after, delimiter, maxKeys) directly inside the read lock, building the
// result from only the entries that survive the filter. At most maxKeys+1
// entries are copied — avoiding the 10K-copy overhead when the bucket is large
// but the page is small.
func (b *inmemBucket) listObjects(prefix, token, startAfter, delim string, maxKeys int) ListObjectsV2Info {
	b.mu.RLock()
	defer b.mu.RUnlock()

	after := startAfter
	if token != "" {
		after = token
	}
	var res ListObjectsV2Info
	res.Objects = make([]ObjectInfo, 0, maxKeys)
	seenPrefix := map[string]bool{}
	for i, key := range b.keys {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}
		if after != "" && key <= after {
			continue
		}
		if delim != "" {
			rest := key[len(prefix):]
			if idx := strings.Index(rest, delim); idx >= 0 {
				cp := prefix + rest[:idx+len(delim)]
				if !seenPrefix[cp] {
					if res.count() >= maxKeys {
						res.IsTruncated = true
						res.NextContinuationToken = lastKey(res)
						return res
					}
					seenPrefix[cp] = true
					res.Prefixes = append(res.Prefixes, cp)
				}
				continue
			}
		}
		if res.count() >= maxKeys {
			res.IsTruncated = true
			res.NextContinuationToken = lastKey(res)
			return res
		}
		if !b.ois[i].DeleteMarker {
			res.Objects = append(res.Objects, b.ois[i])
		}
	}
	return res
}

// inmemIndex is a per-erasure-set in-memory namespace index. It mirrors every
// putObject and deleteObject so that ListObjectsV2 never reads from disk.
//
// Persistence is handled by the bbolt index (index.Index); this layer is the
// fast-read cache in front of it. On startup it is loaded from bbolt so the
// index survives server restarts without a full filesystem scan.
type inmemIndex struct {
	mu      sync.RWMutex
	buckets map[string]*inmemBucket
}

func newInmemIndex() *inmemIndex {
	return &inmemIndex{buckets: make(map[string]*inmemBucket)}
}

// bucket returns the inmemBucket for name, creating it if needed.
func (ix *inmemIndex) bucket(name string) *inmemBucket {
	ix.mu.RLock()
	b, ok := ix.buckets[name]
	ix.mu.RUnlock()
	if ok {
		return b
	}
	ix.mu.Lock()
	// Double-check after acquiring write lock.
	if b, ok = ix.buckets[name]; ok {
		ix.mu.Unlock()
		return b
	}
	b = newInmemBucket()
	ix.buckets[name] = b
	ix.mu.Unlock()
	return b
}

func (ix *inmemIndex) Put(bucketName, key string, oi ObjectInfo) {
	ix.bucket(bucketName).put(key, oi)
}

func (ix *inmemIndex) Delete(bucketName, key string) {
	ix.mu.RLock()
	b, ok := ix.buckets[bucketName]
	ix.mu.RUnlock()
	if ok {
		b.del(key)
	}
}

func (ix *inmemIndex) DeleteBucket(bucketName string) {
	ix.mu.Lock()
	delete(ix.buckets, bucketName)
	ix.mu.Unlock()
}

// AllEntries returns all keys and ObjectInfo values for the bucket in sorted
// key order. Returns (nil, nil) when the bucket has no entries.
func (ix *inmemIndex) AllEntries(bucketName string) ([]string, []ObjectInfo) {
	ix.mu.RLock()
	b, ok := ix.buckets[bucketName]
	ix.mu.RUnlock()
	if !ok {
		return nil, nil
	}
	return b.allEntries()
}

// Populated reports whether the bucket has at least one indexed entry.
func (ix *inmemIndex) Populated(bucketName string) bool {
	ix.mu.RLock()
	b, ok := ix.buckets[bucketName]
	ix.mu.RUnlock()
	if !ok {
		return false
	}
	b.mu.RLock()
	n := len(b.keys)
	b.mu.RUnlock()
	return n > 0
}

// ListObjects applies S3 ListObjectsV2 filters inside the bucket's read lock,
// returning at most maxKeys matching entries without copying the full key set.
// Returns (result, false) when the bucket has no indexed entries — the caller
// should fall back to the filesystem walk path.
func (ix *inmemIndex) ListObjects(bucketName, prefix, token, startAfter, delim string, maxKeys int) (ListObjectsV2Info, bool) {
	ix.mu.RLock()
	b, ok := ix.buckets[bucketName]
	ix.mu.RUnlock()
	if !ok {
		return ListObjectsV2Info{}, false
	}
	b.mu.RLock()
	empty := len(b.keys) == 0
	b.mu.RUnlock()
	if empty {
		return ListObjectsV2Info{}, false
	}
	return b.listObjects(prefix, token, startAfter, delim, maxKeys), true
}

// loadFromBbolt seeds the in-memory index from the persistent bbolt index so
// the server comes up with a warm listing cache after a restart. It is called
// once during pool construction and is a no-op when idx is nil.
func (ix *inmemIndex) loadFromBbolt(bucketName string, keys []string, entries []index.Entry, getOI func(bucket, key string, e index.Entry) ObjectInfo) {
	b := ix.bucket(bucketName)
	b.mu.Lock()
	b.keys = make([]string, len(keys))
	b.ois = make([]ObjectInfo, len(keys))
	copy(b.keys, keys)
	for i, k := range keys {
		b.ois[i] = getOI(bucketName, k, entries[i])
	}
	b.mu.Unlock()
}
