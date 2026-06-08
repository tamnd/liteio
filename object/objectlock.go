// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"encoding/json"
	"path"
	"time"

	"github.com/tamnd/liteio/object/meta"
)

// Object Lock metadata keys stored in FileInfo.Metadata. The values match
// the S3 x-amz-object-lock-* header names, so they round-trip transparently
// through the S3 handler.
const (
	LockModeKey  = "x-amz-object-lock-mode"
	LockUntilKey = "x-amz-object-lock-retain-until-date"
	LegalHoldKey = "x-amz-object-lock-legal-hold"
	LockModeGov  = "GOVERNANCE"
	LockModeComp = "COMPLIANCE"
	LegalHoldOn  = "ON"
	LegalHoldOff = "OFF"
)

// ObjectLockConfig is the bucket-level default retention rule stored under
// .liteio.sys/object-lock as JSON.
type ObjectLockConfig struct {
	Enabled bool              `json:"enabled"`
	Rule    *DefaultRetention `json:"rule,omitempty"`
}

// DefaultRetention is the bucket-level default retention rule.
type DefaultRetention struct {
	Mode  string `json:"mode"`  // GOVERNANCE or COMPLIANCE
	Days  int    `json:"days"`  // Days takes precedence when non-zero
	Years int    `json:"years"` // Years used only when Days is zero
}

// checkObjectLocked returns ErrObjectLocked if the metadata indicates the
// version is protected by retention or legal hold, given the current time.
func checkObjectLocked(m map[string]string, now time.Time) error {
	if m == nil {
		return nil
	}
	if m[LegalHoldKey] == LegalHoldOn {
		return ErrObjectLocked
	}
	mode := m[LockModeKey]
	if mode == "" {
		return nil
	}
	until := m[LockUntilKey]
	if until == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, until)
	if err != nil {
		t, err = time.Parse("2006-01-02T15:04:05.000Z", until)
		if err != nil {
			return nil // unparseable date; skip enforcement
		}
	}
	if now.Before(t) {
		return ErrObjectLocked
	}
	return nil
}

// SetObjectLockConfiguration implements ObjectLayer: it stores the bucket-level
// lock configuration. Versioning must already be enabled on the bucket.
func (sp *ServerPools) SetObjectLockConfiguration(ctx context.Context, bucket string, cfg ObjectLockConfig) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	vc, err := sp.GetBucketVersioning(ctx, bucket)
	if err != nil {
		return err
	}
	if !vc.Enabled {
		return ErrObjectLockRequiresVersioning
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketObjectLock(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetObjectLockConfiguration implements ObjectLayer: it returns the bucket-level
// lock config, or ErrNoSuchObjectLockConfiguration when none is set.
func (sp *ServerPools) GetObjectLockConfiguration(ctx context.Context, bucket string) (ObjectLockConfig, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return ObjectLockConfig{}, err
	}
	doc, err := sp.allSets()[0].bucketObjectLock(ctx, bucket)
	if err != nil {
		return ObjectLockConfig{}, err
	}
	var cfg ObjectLockConfig
	if err := json.Unmarshal(doc, &cfg); err != nil {
		return ObjectLockConfig{}, ErrNoSuchObjectLockConfiguration
	}
	return cfg, nil
}

// SetObjectRetention implements ObjectLayer: it sets the retention mode and
// retain-until date on a specific object version. It enforces that a COMPLIANCE
// lock cannot have its date shortened.
func (sp *ServerPools) SetObjectRetention(ctx context.Context, bucket, object, versionID, mode, retainUntil string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	return sp.route(object).setObjectRetention(ctx, bucket, object, versionID, mode, retainUntil)
}

// GetObjectRetention implements ObjectLayer: it returns the retention mode and
// retain-until date for a specific object version.
func (sp *ServerPools) GetObjectRetention(ctx context.Context, bucket, object, versionID string) (mode, retainUntil string, err error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return "", "", err
	}
	return sp.route(object).getObjectRetention(ctx, bucket, object, versionID)
}

// SetObjectLegalHold implements ObjectLayer: it sets or clears the legal hold
// on a specific object version.
func (sp *ServerPools) SetObjectLegalHold(ctx context.Context, bucket, object, versionID, status string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	return sp.route(object).setObjectLegalHold(ctx, bucket, object, versionID, status)
}

// GetObjectLegalHold implements ObjectLayer: it returns the legal hold status
// for a specific object version.
func (sp *ServerPools) GetObjectLegalHold(ctx context.Context, bucket, object, versionID string) (string, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return "", err
	}
	return sp.route(object).getObjectLegalHold(ctx, bucket, object, versionID)
}

// --- erasureSet helpers --------------------------------------------------

func (s *erasureSet) objectLockPath() string { return path.Join(reserved, "object-lock") }

func (s *erasureSet) setBucketObjectLock(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.objectLockPath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) bucketObjectLock(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.objectLockPath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchObjectLockConfiguration
}

// setObjectRetention updates retention mode and date on a specific version.
func (s *erasureSet) setObjectRetention(ctx context.Context, bucket, object, versionID, mode, retainUntil string) error {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		if !anyPresent(metas) {
			return ErrObjectNotFound
		}
		return ErrReadQuorum
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return ErrObjectNotFound
	}
	pinned := fi.VersionID

	// COMPLIANCE: cannot shorten the retain-until date.
	if fi.Metadata[LockModeKey] == LockModeComp && mode == LockModeComp && retainUntil != "" && fi.Metadata[LockUntilKey] != "" {
		existing, err1 := time.Parse(time.RFC3339Nano, fi.Metadata[LockUntilKey])
		proposed, err2 := time.Parse(time.RFC3339Nano, retainUntil)
		if err1 == nil && err2 == nil && proposed.Before(existing) {
			return ErrComplianceRetentionCannotShorten
		}
	}

	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		versions := metas[i]
		if versions == nil {
			return struct{}{}, nil // drive had no copy; skip (heal will patch it)
		}
		updated := false
		for j := range versions {
			if versions[j].VersionID != pinned {
				continue
			}
			if versions[j].Metadata == nil {
				versions[j].Metadata = map[string]string{}
			}
			if mode == "" {
				delete(versions[j].Metadata, LockModeKey)
				delete(versions[j].Metadata, LockUntilKey)
			} else {
				versions[j].Metadata[LockModeKey] = mode
				versions[j].Metadata[LockUntilKey] = retainUntil
			}
			updated = true
		}
		if !updated {
			return struct{}{}, nil
		}
		raw, err := meta.Marshal(versions)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, object, raw)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) getObjectRetention(ctx context.Context, bucket, object, versionID string) (mode, retainUntil string, err error) {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		if !anyPresent(metas) {
			return "", "", ErrObjectNotFound
		}
		return "", "", ErrReadQuorum
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return "", "", ErrObjectNotFound
	}
	mode = fi.Metadata[LockModeKey]
	if mode == "" {
		return "", "", ErrNoSuchObjectRetention
	}
	return mode, fi.Metadata[LockUntilKey], nil
}

func (s *erasureSet) setObjectLegalHold(ctx context.Context, bucket, object, versionID, status string) error {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		if !anyPresent(metas) {
			return ErrObjectNotFound
		}
		return ErrReadQuorum
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return ErrObjectNotFound
	}
	pinned := fi.VersionID

	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		versions := metas[i]
		if versions == nil {
			return struct{}{}, nil // drive had no copy; skip (heal will patch it)
		}
		updated := false
		for j := range versions {
			if versions[j].VersionID != pinned {
				continue
			}
			if versions[j].Metadata == nil {
				versions[j].Metadata = map[string]string{}
			}
			if status == LegalHoldOff || status == "" {
				delete(versions[j].Metadata, LegalHoldKey)
			} else {
				versions[j].Metadata[LegalHoldKey] = LegalHoldOn
			}
			updated = true
		}
		if !updated {
			return struct{}{}, nil
		}
		raw, err := meta.Marshal(versions)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, object, raw)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) getObjectLegalHold(ctx context.Context, bucket, object, versionID string) (string, error) {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		if !anyPresent(metas) {
			return "", ErrObjectNotFound
		}
		return "", ErrReadQuorum
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return "", ErrObjectNotFound
	}
	status := fi.Metadata[LegalHoldKey]
	if status == "" {
		status = LegalHoldOff
	}
	return status, nil
}
