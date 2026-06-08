// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"encoding/json"
	"path"
	"sync/atomic"
)

// BucketQuota is the bucket-level quota rule stored under .liteio.sys/quota.
// Either or both limits may be non-zero; zero means "no limit".
type BucketQuota struct {
	// HardSize is the hard size limit in bytes. Writes that would push the bucket
	// past this limit are rejected with ErrBucketQuotaExceeded.
	HardSize int64 `json:"hardSize,omitempty"`
	// HardCount is the hard object-count limit. Writes that would exceed this
	// number of objects are rejected.
	HardCount int64 `json:"hardCount,omitempty"`
	// SoftSize is the soft size limit in bytes. Exceeding it is recorded as a
	// warning metric but does not block writes.
	SoftSize int64 `json:"softSize,omitempty"`
	// SoftCount is the soft object-count limit. Exceeding it is recorded as a
	// warning metric but does not block writes.
	SoftCount int64 `json:"softCount,omitempty"`
}

// bucketUsage holds the current usage counters for one bucket. The counters are
// updated inline on every PUT/DELETE so they are always close to current.
// The stats subsystem (scanner, M7+) will replace these with persistent,
// eventually-consistent accounting; the interface stays the same.
type bucketUsage struct {
	size  atomic.Int64
	count atomic.Int64
}

// SetBucketQuota implements ObjectLayer: stores the bucket quota configuration.
func (sp *ServerPools) SetBucketQuota(ctx context.Context, bucket string, q BucketQuota) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	doc, err := json.Marshal(q)
	if err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketQuota(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketQuota implements ObjectLayer: returns the stored quota configuration,
// or ErrNoSuchBucketQuota when none has been set.
func (sp *ServerPools) GetBucketQuota(ctx context.Context, bucket string) (BucketQuota, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return BucketQuota{}, err
	}
	doc, err := sp.allSets()[0].bucketQuota(ctx, bucket)
	if err != nil {
		return BucketQuota{}, err
	}
	var q BucketQuota
	if err := json.Unmarshal(doc, &q); err != nil {
		return BucketQuota{}, ErrNoSuchBucketQuota
	}
	return q, nil
}

// DeleteBucketQuota implements ObjectLayer: removes the quota configuration.
// Idempotent: succeeds even when no quota was set.
func (sp *ServerPools) DeleteBucketQuota(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketQuota(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

// checkBucketQuota enforces the quota for bucket before a write of addSize bytes
// that will result in addCount additional objects. It returns:
//   - nil when the write fits within all configured limits
//   - ErrBucketQuotaExceeded when a hard limit would be crossed
//   - ErrBucketSoftQuotaExceeded when only a soft limit is crossed (the write is
//     still allowed; callers log a warning)
func (sp *ServerPools) checkBucketQuota(ctx context.Context, bucket string, addSize, addCount int64) error {
	doc, err := sp.allSets()[0].bucketQuota(ctx, bucket)
	if err != nil {
		return nil // no quota configured; allow
	}
	var q BucketQuota
	if err := json.Unmarshal(doc, &q); err != nil {
		return nil // malformed config; allow
	}

	cur := sp.loadBucketUsage(bucket)
	curSize := cur.size.Load()
	curCount := cur.count.Load()

	if q.HardSize > 0 && curSize+addSize > q.HardSize {
		return ErrBucketQuotaExceeded
	}
	if q.HardCount > 0 && curCount+addCount > q.HardCount {
		return ErrBucketQuotaExceeded
	}
	if q.SoftSize > 0 && curSize+addSize > q.SoftSize {
		return ErrBucketSoftQuotaExceeded
	}
	if q.SoftCount > 0 && curCount+addCount > q.SoftCount {
		return ErrBucketSoftQuotaExceeded
	}
	return nil
}

// addBucketUsage updates the in-memory usage counters for bucket.
// Call after a successful PUT (+size, +1) or DELETE (-size, -1).
func (sp *ServerPools) addBucketUsage(bucket string, deltaSize, deltaCount int64) {
	u := sp.loadBucketUsage(bucket)
	u.size.Add(deltaSize)
	u.count.Add(deltaCount)
}

// loadBucketUsage returns the existing usage entry for bucket, creating one if
// none exists. The zero value starts both counters at zero.
func (sp *ServerPools) loadBucketUsage(bucket string) *bucketUsage {
	v, _ := sp.usageCache.LoadOrStore(bucket, &bucketUsage{})
	return v.(*bucketUsage)
}

// --- erasureSet helpers --------------------------------------------------

func (s *erasureSet) quotaPath() string { return path.Join(reserved, "quota") }

func (s *erasureSet) setBucketQuota(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.quotaPath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) bucketQuota(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.quotaPath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchBucketQuota
}

func (s *erasureSet) deleteBucketQuota(ctx context.Context, bucket string) error {
	if _, err := s.bucketQuota(ctx, bucket); err != nil {
		return nil // not set; idempotent
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, s.quotaPath(), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}
