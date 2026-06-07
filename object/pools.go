// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"errors"
	"sort"

	"github.com/tamnd/liteio/object/placement"
	"github.com/tamnd/liteio/storage"
)

// SetConfig describes one erasure set: its drives and parity count (M).
type SetConfig struct {
	Drives []storage.StorageAPI
	Parity int
}

// PoolConfig describes one server pool as a list of erasure sets.
type PoolConfig struct {
	Sets []SetConfig
}

// pool is a built server pool: its erasure sets plus a placement weight.
type pool struct {
	sets []*erasureSet
}

// ServerPools is the ObjectLayer implementation. It owns a list of server pools,
// each a list of erasure sets, and routes every object to its set with the
// placement functions (spec docs 04, 11.3). Bucket operations and listings fan
// out across every set, since any object may live in any of them.
type ServerPools struct {
	deploymentID [16]byte
	pools        []*pool
}

var _ ObjectLayer = (*ServerPools)(nil)

// NewServerPools builds the object layer from a per-pool, per-set drive layout.
func NewServerPools(deploymentID [16]byte, pools []PoolConfig) (*ServerPools, error) {
	if len(pools) == 0 {
		return nil, ErrInvalidArgument
	}
	sp := &ServerPools{deploymentID: deploymentID}
	for _, pc := range pools {
		if len(pc.Sets) == 0 {
			return nil, ErrInvalidArgument
		}
		p := &pool{}
		for _, sc := range pc.Sets {
			set, err := newSet(sc.Drives, sc.Parity, deploymentID)
			if err != nil {
				return nil, err
			}
			p.sets = append(p.sets, set)
		}
		sp.pools = append(sp.pools, p)
	}
	return sp, nil
}

// NewSingleSet is a convenience constructor for one pool of one set, the common
// single-node small deployment and the shape most tests use.
func NewSingleSet(deploymentID [16]byte, drives []storage.StorageAPI, parity int) (*ServerPools, error) {
	return NewServerPools(deploymentID, []PoolConfig{{Sets: []SetConfig{{Drives: drives, Parity: parity}}}})
}

// route returns the erasure set that owns object. Pool selection is free-space
// weighted (flat until live free-space reporting lands); set selection is the
// deployment-keyed SipHash from placement.
func (sp *ServerPools) route(object string) *erasureSet {
	poolFree := make([]placement.PoolFreeInfo, len(sp.pools))
	for i := range sp.pools {
		poolFree[i] = placement.PoolFreeInfo{Index: i, Free: 1}
	}
	pi := placement.PoolIndex(object, poolFree)
	p := sp.pools[pi]
	si := placement.SetIndex(object, sp.deploymentID, len(p.sets))
	return p.sets[si]
}

// allSets returns every set across every pool.
func (sp *ServerPools) allSets() []*erasureSet {
	var out []*erasureSet
	for _, p := range sp.pools {
		out = append(out, p.sets...)
	}
	return out
}

// --- buckets (fan out to every set) --------------------------------------

// MakeBucket implements ObjectLayer: it creates the bucket on every set.
func (sp *ServerPools) MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	existsAll := true
	for _, set := range sp.allSets() {
		err := set.makeBucket(ctx, bucket, opts.VersionedDefault)
		if err == nil {
			existsAll = false
			continue
		}
		if errors.Is(err, ErrBucketExists) {
			continue
		}
		return err
	}
	if existsAll {
		return ErrBucketExists
	}
	return nil
}

// GetBucketInfo implements ObjectLayer.
func (sp *ServerPools) GetBucketInfo(ctx context.Context, bucket string) (BucketInfo, error) {
	return sp.allSets()[0].getBucketInfo(ctx, bucket)
}

// ListBuckets implements ObjectLayer.
func (sp *ServerPools) ListBuckets(ctx context.Context) ([]BucketInfo, error) {
	return sp.allSets()[0].listBuckets(ctx)
}

// DeleteBucket implements ObjectLayer: it removes the bucket from every set.
func (sp *ServerPools) DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error {
	var firstErr error
	for _, set := range sp.allSets() {
		if err := set.deleteBucket(ctx, bucket, opts.Force); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// --- objects (route to the owning set) -----------------------------------

// PutObject implements ObjectLayer: it routes the object to its set and writes it.
func (sp *ServerPools) PutObject(ctx context.Context, bucket, object string, r *PutReader, opts ObjectOptions) (ObjectInfo, error) {
	return sp.route(object).putObject(ctx, bucket, object, r, opts)
}

// GetObject implements ObjectLayer: it routes to the owning set and reads the object.
func (sp *ServerPools) GetObject(ctx context.Context, bucket, object string, opts ObjectOptions) (*GetObjectReader, error) {
	return sp.route(object).getObject(ctx, bucket, object, opts)
}

// GetObjectInfo implements ObjectLayer.
func (sp *ServerPools) GetObjectInfo(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error) {
	return sp.route(object).getObjectInfo(ctx, bucket, object, opts)
}

// DeleteObject implements ObjectLayer.
func (sp *ServerPools) DeleteObject(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error) {
	return sp.route(object).deleteObject(ctx, bucket, object, opts)
}

// DeleteObjects implements ObjectLayer: it deletes a batch of keys, returning per-key results.
func (sp *ServerPools) DeleteObjects(ctx context.Context, bucket string, objs []ObjectToDelete, opts ObjectOptions) ([]DeletedObject, []error) {
	deleted := make([]DeletedObject, len(objs))
	errs := make([]error, len(objs))
	for i, o := range objs {
		info, err := sp.route(o.Name).deleteObject(ctx, bucket, o.Name, ObjectOptions{VersionID: o.VersionID})
		errs[i] = err
		if err == nil {
			deleted[i] = DeletedObject{
				Name:         o.Name,
				VersionID:    o.VersionID,
				DeleteMarker: info.DeleteMarker,
			}
			if info.DeleteMarker {
				deleted[i].DeleteMarkerVersionID = info.VersionID
			}
		}
	}
	return deleted, errs
}

// --- listing (union across sets) -----------------------------------------

// ListObjectsV2 implements ObjectLayer: it unions object keys across sets and pages them.
func (sp *ServerPools) ListObjectsV2(ctx context.Context, bucket, prefix, token, startAfter, delim string, maxKeys int, _ bool) (ListObjectsV2Info, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return ListObjectsV2Info{}, err
	}
	if maxKeys <= 0 {
		maxKeys = 1000
	}
	seen := map[string]bool{}
	var keys []string
	for _, set := range sp.allSets() {
		ks, err := set.walkObjects(ctx, bucket)
		if err != nil {
			continue
		}
		for _, k := range ks {
			if !seen[k] {
				seen[k] = true
				keys = append(keys, k)
			}
		}
	}
	sort.Strings(keys)
	getInfo := func(key string) (ObjectInfo, error) {
		return sp.route(key).getObjectInfo(ctx, bucket, key, ObjectOptions{})
	}
	return applyListing(keys, prefix, token, startAfter, delim, maxKeys, getInfo), nil
}
