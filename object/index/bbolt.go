// SPDX-License-Identifier: Apache-2.0

// Package index implements a persistent, per-erasure-set namespace index backed
// by bbolt. It stores one entry per object key, keyed by the UTF-8 object key,
// with a msgpack-encoded Entry as the value. bbolt's B-tree layout means a full
// bucket scan (LIST) is a sequential cursor walk — orders of magnitude faster
// than a recursive filesystem readdir on slow block storage.
//
// The index is a write-through cache: every successful putObject/deleteObject in
// the erasure set updates it. A warm index replaces the filesystem walk in
// walkObjects; a cold or missing index falls back gracefully to the filesystem.
//
// Write batching: Put and Delete use db.Batch, which lets bbolt coalesce
// concurrent callers into a single fsync. At 100 concurrent PUTs the 100 index
// writes collapse to one transaction commit — group commit at zero code cost.
package index

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5"
	bolt "go.etcd.io/bbolt"
)

// Entry holds the metadata stored per object key in the index. UserDefined is
// omitted for empty maps (omitempty) to keep the value compact for typical
// objects with no custom headers.
type Entry struct {
	ETag        string            `msgpack:"e"`
	Size        int64             `msgpack:"s"`
	ModTime     time.Time         `msgpack:"m"`
	ContentType string            `msgpack:"ct,omitempty"`
	VersionID   string            `msgpack:"v,omitempty"`
	UserDefined map[string]string `msgpack:"u,omitempty"`
}

// Index is a persistent namespace index backed by a bbolt database. One Index
// serves one erasure set; it is opened at node start and closed at shutdown.
type Index struct {
	db *bolt.DB
}

// Open opens (or creates) the bbolt index database at dir/index.db.
func Open(dir string) (*Index, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("index: mkdir %s: %w", dir, err)
	}
	db, err := bolt.Open(filepath.Join(dir, "index.db"), 0o600, &bolt.Options{
		Timeout:      2 * time.Second,
		FreelistType: bolt.FreelistArrayType,
	})
	if err != nil {
		return nil, fmt.Errorf("index: open: %w", err)
	}
	return &Index{db: db}, nil
}

// Put upserts entry for the given bucket/key. It uses db.Batch so concurrent
// callers are coalesced into a single transaction commit.
func (ix *Index) Put(bucket, key string, e Entry) error {
	return ix.db.Batch(func(tx *bolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucket))
		if err != nil {
			return err
		}
		v, err := msgpack.Marshal(&e)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), v)
	})
}

// Delete removes the key from the given bucket. It is a no-op when the key or
// bucket does not exist.
func (ix *Index) Delete(bucket, key string) error {
	return ix.db.Batch(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		return b.Delete([]byte(key))
	})
}

// AllKeys returns every object key in bucket in lexicographic order. It is the
// fast path for walkObjects: one cursor sweep instead of one readdir per object.
// A missing bbolt bucket means no objects have been indexed yet; return empty.
func (ix *Index) AllKeys(bucket string) ([]string, error) {
	var keys []string
	err := ix.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		keys = make([]string, 0, b.Stats().KeyN)
		return b.ForEach(func(k, _ []byte) error {
			keys = append(keys, string(k))
			return nil
		})
	})
	return keys, err
}

// AllEntries returns all entries in bucket in lexicographic key order, along
// with their keys. Used by list-with-metadata paths.
func (ix *Index) AllEntries(bucket string) ([]string, []Entry, error) {
	var keys []string
	var entries []Entry
	err := ix.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucket))
		if b == nil {
			return nil
		}
		n := b.Stats().KeyN
		keys = make([]string, 0, n)
		entries = make([]Entry, 0, n)
		return b.ForEach(func(k, v []byte) error {
			var e Entry
			if err := msgpack.Unmarshal(v, &e); err != nil {
				return nil // skip corrupt entries
			}
			keys = append(keys, string(k))
			entries = append(entries, e)
			return nil
		})
	})
	return keys, entries, err
}

// CreateBucket ensures the bucket exists in the index. Called on MakeBucket.
func (ix *Index) CreateBucket(bucket string) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists([]byte(bucket))
		return err
	})
}

// DeleteBucket removes the entire bucket from the index. Called on DeleteBucket.
func (ix *Index) DeleteBucket(bucket string) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		err := tx.DeleteBucket([]byte(bucket))
		if err == bolt.ErrBucketNotFound {
			return nil
		}
		return err
	})
}

// HasBucket reports whether the bucket exists in the index.
func (ix *Index) HasBucket(bucket string) bool {
	var found bool
	_ = ix.db.View(func(tx *bolt.Tx) error {
		found = tx.Bucket([]byte(bucket)) != nil
		return nil
	})
	return found
}

// ListBuckets returns all bucket names stored in the index.
func (ix *Index) ListBuckets() ([]string, error) {
	var buckets []string
	err := ix.db.View(func(tx *bolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bolt.Bucket) error {
			s := string(name)
			if !strings.HasPrefix(s, ".") {
				buckets = append(buckets, s)
			}
			return nil
		})
	})
	return buckets, err
}

// Rebuild repopulates the index for a bucket from a set of keys and their
// entries, replacing whatever was stored before. Used for recovery.
func (ix *Index) Rebuild(bucket string, keys []string, entries []Entry) error {
	return ix.db.Update(func(tx *bolt.Tx) error {
		_ = tx.DeleteBucket([]byte(bucket))
		b, err := tx.CreateBucket([]byte(bucket))
		if err != nil {
			return err
		}
		for i, k := range keys {
			v, err := msgpack.Marshal(&entries[i])
			if err != nil {
				continue
			}
			if err := b.Put([]byte(k), v); err != nil {
				return err
			}
		}
		return nil
	})
}

// Close closes the database.
func (ix *Index) Close() error { return ix.db.Close() }
