// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/event"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/tier"
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
	cache        *metacache

	// kms, when non-nil, is the key-management backend used for SSE-S3 and
	// SSE-KMS encryption. A nil kms means server-managed encryption is
	// disabled; objects with explicit SSE-C keys still work.
	kms KMSBackend

	// nodeID identifies this node as the owner of the namespace locks it takes;
	// nsLockers is the lock-server quorum guarding the namespace. A single-node
	// deployment holds one in-process LocalLocker; a clustered deployment replaces
	// this with the cluster's lock peers (reached over cluster/rpc) behind the same
	// Locker contract.
	nodeID    string
	nsLockers []lock.Locker

	// mrf is the reactive-heal queue. The write path enqueues an object that
	// reached quorum but missed a drive; StartHealing's worker drains it and
	// repairs the laggards. It is created in the constructor so enqueues are always
	// counted, but only drained once StartHealing runs.
	mrf *mrf

	// cacheNotify, when set, is called best-effort after a local mutation so peers
	// can keep their listing caches coherent. It is nil for a single-node
	// deployment (no peers to tell) and installed by WithCacheNotifier in a
	// cluster. A non-empty key was added by a write; an empty key means the bucket
	// was invalidated by a delete.
	cacheNotify CacheNotifier

	// usageCache tracks per-bucket object counts and sizes in memory. It is
	// updated inline on every PUT/DELETE, giving the quota enforcement an always
	// current (though eventually replaced by the scanner) view of usage.
	usageCache sync.Map // map[string]*bucketUsage

	// dispatcher, when non-nil, is the event dispatcher for bucket notification
	// delivery. Nil disables event notifications (no-op).
	dispatcher *event.Dispatcher

	// nowFn overrides time.Now for testing. Nil uses time.Now.
	nowFn func() time.Time

	// tierMu guards tierClients.
	tierMu      sync.RWMutex
	tierClients map[string]tier.RemoteClient // keyed by tier name

	// Rebalance and decommission state.
	rbSt     rbState          // counters for the cluster-wide rebalance walk
	dcMu     sync.RWMutex     // guards dcState and draining
	dcState  map[int]*rbState // per-pool decommission state
	draining map[int]bool     // pools marked as draining (excluded from routing)
	fc       *poolFreeCache   // per-pool free-space cache (refreshed every 30 s)
}

// CacheNotifier carries a node's local metacache events to its peers so listings
// served anywhere reflect a write made anywhere. A non-empty key was added; an
// empty key invalidates the whole bucket.
type CacheNotifier func(bucket, key string)

var _ ObjectLayer = (*ServerPools)(nil)

// Option configures a ServerPools at construction. Options are applied after the
// single-node defaults, so an option overrides them.
type Option func(*ServerPools)

// WithLockers installs the distributed namespace-lock quorum: nodeID is this
// node's identity as the owner of the locks it takes, and lockers is the set of
// lock authorities (this node's own plus its peers) that a namespace lock spans.
// Without this option a ServerPools uses one in-process LocalLocker, which is the
// correct single-node behavior. A nil or empty lockers leaves the default in
// place.
func WithLockers(nodeID string, lockers []lock.Locker) Option {
	return func(sp *ServerPools) {
		if len(lockers) == 0 {
			return
		}
		sp.nodeID = nodeID
		sp.nsLockers = lockers
	}
}

// WithCacheNotifier installs the notifier the layer calls after a local mutation
// so peers can keep their listing caches coherent. Without it the layer makes no
// cross-node notifications, which is correct for a single-node deployment. A nil
// notifier leaves the default (no notification) in place.
func WithCacheNotifier(n CacheNotifier) Option {
	return func(sp *ServerPools) {
		if n == nil {
			return
		}
		sp.cacheNotify = n
	}
}

// WithDispatcher installs the event dispatcher used for bucket notification
// delivery. Without it, event notifications are disabled (no-op). Nil is safe.
func WithDispatcher(d *event.Dispatcher) Option {
	return func(sp *ServerPools) { sp.dispatcher = d }
}

// now returns the current time, using the injected clock if set.
func (sp *ServerPools) now() time.Time {
	if sp.nowFn != nil {
		return sp.nowFn()
	}
	return time.Now()
}

// NewServerPools builds the object layer from a per-pool, per-set drive layout.
func NewServerPools(deploymentID [16]byte, pools []PoolConfig, opts ...Option) (*ServerPools, error) {
	if len(pools) == 0 {
		return nil, ErrInvalidArgument
	}
	sp := &ServerPools{
		deploymentID: deploymentID,
		cache:        newMetacache(),
		nodeID:       "local",
		nsLockers:    []lock.Locker{lock.NewLocalLocker("local")},
		tierClients:  make(map[string]tier.RemoteClient),
		dcState:      make(map[int]*rbState),
		draining:     make(map[int]bool),
		fc:           newPoolFreeCache(),
	}
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
	for _, opt := range opts {
		opt(sp)
	}

	// Wire reactive heal: each set reports a partial write to the queue, which
	// routes the heal back through the layer so it lands on the right set.
	sp.mrf = newMRF(sp.healTask, DefaultMRFDepth)
	for _, set := range sp.allSets() {
		set.notifyPartial = func(bucket, object, versionID string) {
			sp.mrf.enqueue(healTask{bucket: bucket, object: object, versionID: versionID})
		}
		set.kms = sp.kms
	}
	return sp, nil
}

// StartHealing launches the reactive-heal worker, which drains the
// most-recently-failed queue until ctx is cancelled. Call it once after
// construction; a deployment that never starts it still records dropped tasks but
// performs no reactive repair (the proactive scanner remains the durable
// backstop).
func (sp *ServerPools) StartHealing(ctx context.Context) {
	go sp.mrf.run(ctx)
}

// MRFStats returns the reactive-heal queue's running counters.
func (sp *ServerPools) MRFStats() MRFStats {
	return MRFStats{
		Pending: int64(len(sp.mrf.tasks)),
		Dropped: sp.mrf.dropped.Load(),
		Healed:  sp.mrf.healed.Load(),
		Failed:  sp.mrf.failed.Load(),
	}
}

// healTask routes a queued heal back to the set that owns the object and repairs
// the version there.
func (sp *ServerPools) healTask(ctx context.Context, t healTask) error {
	return sp.route(t.object).healObject(ctx, t.bucket, t.object, t.versionID)
}

// notifyCache tells the peers, if any, about a local metacache event. It is
// best-effort: the notifier never blocks the caller and a lost notification is
// covered by the metacache TTL, so the write path is never slowed or failed by
// cross-node coherence.
func (sp *ServerPools) notifyCache(bucket, key string) {
	if sp.cacheNotify != nil {
		sp.cacheNotify(bucket, key)
	}
}

// ApplyRemoteCache applies a peer's metacache event to this node so a listing
// served here reflects a write made there. A non-empty key is recorded as
// present (keeping a warm cache correct without a re-walk); an empty key
// invalidates the bucket so the next list re-reads the tree. It is the inbound
// half of WithCacheNotifier and a no-op for any bucket this node has not cached.
func (sp *ServerPools) ApplyRemoteCache(bucket, key string) {
	if key == "" {
		sp.cache.invalidate(bucket)
		return
	}
	sp.cache.add(bucket, key)
}

// NewSingleSet is a convenience constructor for one pool of one set, the common
// single-node small deployment and the shape most tests use.
func NewSingleSet(deploymentID [16]byte, drives []storage.StorageAPI, parity int, opts ...Option) (*ServerPools, error) {
	return NewServerPools(deploymentID, []PoolConfig{{Sets: []SetConfig{{Drives: drives, Parity: parity}}}}, opts...)
}

// route returns the erasure set that owns object using free-space-weighted pool
// selection. The free-space data is read from a 30 s cache populated by poolFreeInfo;
// draining pools are excluded so they receive no new writes.
func (sp *ServerPools) route(object string) *erasureSet {
	return sp.routeWithFree(context.Background(), object)
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
	sp.cache.invalidate(bucket)
	sp.notifyCache(bucket, "")
	return firstErr
}

// --- objects (route to the owning set) -----------------------------------

// PutObject implements ObjectLayer: it routes the object to its set and writes it.
func (sp *ServerPools) PutObject(ctx context.Context, bucket, object string, r *PutReader, opts ObjectOptions) (ObjectInfo, error) {
	unlock, err := sp.lockObject(ctx, bucket, object)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer unlock()

	// Quota check: hard limits block the write; soft limits are non-blocking but
	// callers should record a warning. ErrBucketSoftQuotaExceeded is not fatal.
	addSize := r.Size
	if addSize < 0 {
		addSize = 0
	}
	if qErr := sp.checkBucketQuota(ctx, bucket, addSize, 1); qErr != nil && qErr != ErrBucketSoftQuotaExceeded {
		return ObjectInfo{}, qErr
	}

	oi, err := sp.route(object).putObject(ctx, bucket, object, r, opts)
	if err == nil {
		sp.cache.add(bucket, object)
		sp.notifyCache(bucket, object)
		sp.addBucketUsage(bucket, oi.Size, 1)
		name := event.EventName(opts.EventName)
		if name == "" {
			name = event.ObjectCreatedPut
		}
		sp.notifyPut(ctx, oi, name, opts.SourceIP)
		sp.maybeReplicate(ctx, bucket, object, oi)
	}
	return oi, err
}

// GetObject implements ObjectLayer: it routes to the owning set and reads the
// object. For objects transitioned to a remote tier (Tier field non-empty), it
// fetches the data from the remote tier client instead of local drives.
func (sp *ServerPools) GetObject(ctx context.Context, bucket, object string, opts ObjectOptions) (*GetObjectReader, error) {
	// Peek at the object info to detect tier stubs before reading shards.
	oi, err := sp.route(object).getObjectInfo(ctx, bucket, object, opts)
	if err != nil {
		return nil, err
	}
	if oi.Tier != "" && oi.RestoreExpires.IsZero() && !oi.RestoreOngoing {
		// Data lives on the remote tier; fetch transparently.
		return sp.getObjectTiered(ctx, oi, opts)
	}
	return sp.route(object).getObject(ctx, bucket, object, opts)
}

// GetObjectInfo implements ObjectLayer.
func (sp *ServerPools) GetObjectInfo(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error) {
	return sp.route(object).getObjectInfo(ctx, bucket, object, opts)
}

// DeleteObject implements ObjectLayer.
func (sp *ServerPools) DeleteObject(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error) {
	unlock, err := sp.lockObject(ctx, bucket, object)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer unlock()
	// Read size before deleting so the usage counter can be decremented.
	var prevSize int64
	if prev, infoErr := sp.route(object).getObjectInfo(ctx, bucket, object, opts); infoErr == nil {
		prevSize = prev.Size
	}
	oi, err := sp.route(object).deleteObject(ctx, bucket, object, opts)
	if err == nil {
		sp.cache.invalidate(bucket)
		sp.notifyCache(bucket, "")
		if !oi.DeleteMarker {
			sp.addBucketUsage(bucket, -prevSize, -1)
		}
		sp.notifyDelete(ctx, bucket, object, oi.VersionID, oi.DeleteMarker, opts.SourceIP)
		sp.maybeReplicateDelete(ctx, bucket, object, oi.VersionID, oi.DeleteMarker)
	}
	return oi, err
}

// DeleteObjects implements ObjectLayer: it deletes a batch of keys, returning per-key results.
func (sp *ServerPools) DeleteObjects(ctx context.Context, bucket string, objs []ObjectToDelete, opts ObjectOptions) ([]DeletedObject, []error) {
	deleted := make([]DeletedObject, len(objs))
	errs := make([]error, len(objs))
	for i, o := range objs {
		unlock, err := sp.lockObject(ctx, bucket, o.Name)
		if err != nil {
			errs[i] = err
			continue
		}
		info, err := sp.route(o.Name).deleteObject(ctx, bucket, o.Name, ObjectOptions{VersionID: o.VersionID})
		unlock()
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
	sp.cache.invalidate(bucket)
	sp.notifyCache(bucket, "")
	return deleted, errs
}

// --- multipart upload (route to the owning set) --------------------------

// NewMultipartUpload implements ObjectLayer.
func (sp *ServerPools) NewMultipartUpload(ctx context.Context, bucket, object string, opts ObjectOptions) (string, error) {
	return sp.route(object).newMultipartUpload(ctx, bucket, object, opts)
}

// PutObjectPart implements ObjectLayer.
func (sp *ServerPools) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, r *PutReader, opts ObjectOptions) (PartInfo, error) {
	return sp.route(object).putObjectPart(ctx, bucket, object, uploadID, partID, r, opts)
}

// CompleteMultipartUpload implements ObjectLayer.
func (sp *ServerPools) CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, parts []CompletePart, opts ObjectOptions) (ObjectInfo, error) {
	unlock, err := sp.lockObject(ctx, bucket, object)
	if err != nil {
		return ObjectInfo{}, err
	}
	defer unlock()
	oi, err := sp.route(object).completeMultipartUpload(ctx, bucket, object, uploadID, parts, opts)
	if err == nil {
		sp.cache.add(bucket, object)
		sp.notifyCache(bucket, object)
		sp.addBucketUsage(bucket, oi.Size, 1)
		sp.notifyPut(ctx, oi, event.ObjectCreatedCompleteMultipartUpload, opts.SourceIP)
	}
	return oi, err
}

// AbortMultipartUpload implements ObjectLayer.
func (sp *ServerPools) AbortMultipartUpload(ctx context.Context, bucket, object, uploadID string, _ ObjectOptions) error {
	return sp.route(object).abortMultipartUpload(ctx, bucket, uploadID)
}

// ListObjectParts implements ObjectLayer.
func (sp *ServerPools) ListObjectParts(ctx context.Context, bucket, object, uploadID string, partNumberMarker, maxParts int, _ ObjectOptions) (ListPartsInfo, error) {
	return sp.route(object).listObjectParts(ctx, bucket, object, uploadID, partNumberMarker, maxParts)
}

// ListMultipartUploads implements ObjectLayer: it unions in-progress uploads
// across every set and pages them by (object key, upload id).
func (sp *ServerPools) ListMultipartUploads(ctx context.Context, bucket, prefix, keyMarker, uploadIDMarker, delimiter string, maxUploads int) (ListMultipartsInfo, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return ListMultipartsInfo{}, err
	}
	if maxUploads <= 0 {
		maxUploads = 1000
	}
	var all []MultipartUpload
	for _, set := range sp.allSets() {
		ups, err := set.listMultipartUploads(ctx, bucket, prefix)
		if err != nil {
			continue
		}
		all = append(all, ups...)
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].Object != all[j].Object {
			return all[i].Object < all[j].Object
		}
		return all[i].UploadID < all[j].UploadID
	})

	out := ListMultipartsInfo{
		Bucket:         bucket,
		Prefix:         prefix,
		Delimiter:      delimiter,
		KeyMarker:      keyMarker,
		UploadIDMarker: uploadIDMarker,
		MaxUploads:     maxUploads,
	}
	for _, up := range all {
		if !afterUploadMarker(up, keyMarker, uploadIDMarker) {
			continue
		}
		if len(out.Uploads) >= maxUploads {
			out.IsTruncated = true
			out.NextKeyMarker = out.Uploads[len(out.Uploads)-1].Object
			out.NextUploadIDMarker = out.Uploads[len(out.Uploads)-1].UploadID
			break
		}
		out.Uploads = append(out.Uploads, up)
	}
	return out, nil
}

// afterUploadMarker reports whether up sorts strictly after the (keyMarker,
// uploadIDMarker) pagination cursor.
func afterUploadMarker(up MultipartUpload, keyMarker, uploadIDMarker string) bool {
	if keyMarker == "" {
		return true
	}
	if up.Object != keyMarker {
		return up.Object > keyMarker
	}
	return up.UploadID > uploadIDMarker
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
	keys, err := sp.bucketKeys(ctx, bucket)
	if err != nil {
		return ListObjectsV2Info{}, err
	}
	getInfo := func(key string) (ObjectInfo, error) {
		return sp.route(key).getObjectInfo(ctx, bucket, key, ObjectOptions{})
	}
	return applyListing(keys, prefix, token, startAfter, delim, maxKeys, getInfo), nil
}

// bucketKeys returns the bucket's sorted, de-duplicated object-key union across
// every set, served from the metacache (populated by walkUnion on a miss). A
// paginated client reuses one walk across pages; an unchanged bucket reuses it
// across repeat lists (spec doc 07.5).
func (sp *ServerPools) bucketKeys(ctx context.Context, bucket string) ([]string, error) {
	return sp.cache.keys(bucket, func() ([]string, error) {
		return sp.walkUnion(ctx, bucket), nil
	})
}

// walkUnion descends every set's namespace and returns the sorted union of keys.
func (sp *ServerPools) walkUnion(ctx context.Context, bucket string) []string {
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
	return keys
}
