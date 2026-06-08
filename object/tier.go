// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path"
	"time"

	"github.com/tamnd/liteio/object/meta"
	"github.com/tamnd/liteio/tier"
)

// sysVol is the synthetic volume (directory) used to store global (non-bucket)
// configuration: tier configs, etc. It is created on first write.
const sysVol = ".liteio-sys"

// --- ServerPools fields (added) -----------------------------------------
//
// The following fields are logically part of ServerPools but defined here for
// readability. They are accessed through the tierMu / tierClients pair.
//
// In pools.go ServerPools now has:
//   tierMu      sync.RWMutex
//   tierClients map[string]tier.RemoteClient // keyed by tier name
//
// These are defined in pools.go via the poolsTierFields struct embed; this file
// only uses them.

// --- tier config CRUD ---------------------------------------------------

// ensureSysVol creates the sysVol volume on all online drives so WriteMeta calls
// inside that volume succeed.
func (sp *ServerPools) ensureSysVol(ctx context.Context) {
	for _, set := range sp.allSets() {
		for _, d := range set.drives {
			if d.IsOnline() {
				_ = d.MakeVol(ctx, sysVol)
			}
		}
	}
}

// loadTierConfigs reads the tier config JSON from the first available drive.
func (sp *ServerPools) loadTierConfigs(ctx context.Context) ([]tier.TierConfig, error) {
	for _, set := range sp.allSets() {
		for _, d := range set.drives {
			if !d.IsOnline() {
				continue
			}
			data, err := d.ReadMeta(ctx, sysVol, "tier")
			if err != nil {
				continue
			}
			var cfgs []tier.TierConfig
			if err := json.Unmarshal(data, &cfgs); err != nil {
				continue
			}
			return cfgs, nil
		}
	}
	return nil, ErrNoSuchTierConfig
}

// saveTierConfigs fans out the tier config JSON to every drive in every set.
func (sp *ServerPools) saveTierConfigs(ctx context.Context, cfgs []tier.TierConfig) error {
	sp.ensureSysVol(ctx)
	data, err := json.Marshal(cfgs)
	if err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		res := fanOut(ctx, len(set.drives), func(ctx context.Context, i int) (struct{}, error) {
			return struct{}{}, set.drives[i].WriteMeta(ctx, sysVol, "tier", data)
		})
		if countOK(res) < set.writeQuorum() {
			return ErrWriteQuorum
		}
	}
	return nil
}

// SetTierConfig implements ObjectLayer: adds or replaces a remote tier config.
func (sp *ServerPools) SetTierConfig(ctx context.Context, cfg tier.TierConfig) error {
	if err := tier.Validate(cfg); err != nil {
		return err
	}
	cfgs, err := sp.loadTierConfigs(ctx)
	if err != nil && err != ErrNoSuchTierConfig {
		return err
	}
	// Replace existing or append.
	replaced := false
	for i, c := range cfgs {
		if c.Name == cfg.Name {
			cfgs[i] = cfg
			replaced = true
			break
		}
	}
	if !replaced {
		cfgs = append(cfgs, cfg)
	}
	if err := sp.saveTierConfigs(ctx, cfgs); err != nil {
		return err
	}
	// Refresh the client cache.
	client, clientErr := tier.NewClient(cfg)
	if clientErr == nil {
		sp.tierMu.Lock()
		sp.tierClients[cfg.Name] = client
		sp.tierMu.Unlock()
	}
	return nil
}

// GetTierConfig implements ObjectLayer: returns the named tier config.
func (sp *ServerPools) GetTierConfig(ctx context.Context, name string) (tier.TierConfig, error) {
	cfgs, err := sp.loadTierConfigs(ctx)
	if err != nil {
		return tier.TierConfig{}, err
	}
	for _, c := range cfgs {
		if c.Name == name {
			return c, nil
		}
	}
	return tier.TierConfig{}, ErrNoSuchTierConfig
}

// ListTierConfigs implements ObjectLayer: returns all configured tiers.
func (sp *ServerPools) ListTierConfigs(ctx context.Context) ([]tier.TierConfig, error) {
	cfgs, err := sp.loadTierConfigs(ctx)
	if err == ErrNoSuchTierConfig {
		return nil, nil // empty list, not error
	}
	return cfgs, err
}

// DeleteTierConfig implements ObjectLayer: removes the named tier config.
func (sp *ServerPools) DeleteTierConfig(ctx context.Context, name string) error {
	cfgs, err := sp.loadTierConfigs(ctx)
	if err != nil {
		return err
	}
	updated := cfgs[:0]
	for _, c := range cfgs {
		if c.Name != name {
			updated = append(updated, c)
		}
	}
	if len(updated) == len(cfgs) {
		return ErrNoSuchTierConfig
	}
	if err := sp.saveTierConfigs(ctx, updated); err != nil {
		return err
	}
	sp.tierMu.Lock()
	delete(sp.tierClients, name)
	sp.tierMu.Unlock()
	return nil
}

// tierClient returns a cached RemoteClient for the named tier, initializing it
// lazily from the persisted config if needed.
func (sp *ServerPools) tierClient(ctx context.Context, name string) (tier.RemoteClient, error) {
	sp.tierMu.RLock()
	c, ok := sp.tierClients[name]
	sp.tierMu.RUnlock()
	if ok {
		return c, nil
	}
	// Not in cache; load config and build client.
	cfg, err := sp.GetTierConfig(ctx, name)
	if err != nil {
		return nil, err
	}
	client, err := tier.NewClient(cfg)
	if err != nil {
		return nil, err
	}
	sp.tierMu.Lock()
	sp.tierClients[name] = client
	sp.tierMu.Unlock()
	return client, nil
}

// --- TransitionObject ---------------------------------------------------

// TransitionObject moves an object's data to a remote tier, replacing the local
// data with a tier stub in obj.meta. After transition the object remains
// GET-able via the transparent read path (getObjectTiered).
//
// This is called by the lifecycle scanner (§9.2) and by admin commands. It is NOT
// the S3 public API — there is no direct S3 call that triggers a transition.
func (sp *ServerPools) TransitionObject(ctx context.Context, bucket, object, tierName string, opts ObjectOptions) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	cfg, err := sp.GetTierConfig(ctx, tierName)
	if err != nil {
		return err
	}
	client, err := sp.tierClient(ctx, tierName)
	if err != nil {
		return err
	}

	set := sp.route(object)

	// Read the current object data from local drives.
	reader, err := set.getObject(ctx, bucket, object, opts)
	if err != nil {
		return err
	}
	defer reader.Close() //nolint:errcheck
	oi := reader.ObjectInfo

	// Compute the remote key: prefix/bucket/versionID/object or prefix/bucket/object.
	prefix := ""
	if cfg.S3 != nil {
		prefix = cfg.S3.Prefix
	}
	remoteKey := tier.RemoteKey(prefix, bucket, object, oi.VersionID)

	// PUT data to the remote tier.
	if err := client.Put(ctx, remoteKey, reader, oi.Size); err != nil {
		return fmt.Errorf("object: transition to tier %q: %w", tierName, err)
	}

	// Update obj.meta on every drive to record the tier stub and delete local data.
	return set.applyTierStub(ctx, bucket, object, oi.VersionID, tierName, remoteKey)
}

// applyTierStub updates the obj.meta on every drive to add the tier metadata keys
// (MetaName and MetaKey), then deletes the data shard files so local storage is freed.
func (s *erasureSet) applyTierStub(ctx context.Context, bucket, object, versionID, tierName, remoteKey string) error {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		return ErrObjectNotFound
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return ErrObjectNotFound
	}
	pinned := fi.VersionID

	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		versions := metas[i]
		if versions == nil {
			return struct{}{}, nil // offline drive; heal will patch
		}
		for j := range versions {
			if versions[j].VersionID != pinned {
				continue
			}
			if versions[j].Metadata == nil {
				versions[j].Metadata = map[string]string{}
			}
			versions[j].Metadata[tier.MetaName] = tierName
			versions[j].Metadata[tier.MetaKey] = remoteKey
			// Clear inline data — the object is now remote.
			versions[j].InlineData = nil
			raw, err := meta.Marshal(versions)
			if err != nil {
				return struct{}{}, err
			}
			if err := s.drives[i].WriteMeta(ctx, bucket, object, raw); err != nil {
				return struct{}{}, err
			}
			// Delete the data shard directory (best effort; local data freed).
			_ = s.drives[i].Delete(ctx, bucket, path.Join(object, versionDir(pinned)), true)
			return struct{}{}, nil
		}
		return struct{}{}, nil // version not on this drive
	})
	if countOK(writes) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

// --- RestoreObject ------------------------------------------------------

// RestoreObject rehydrates a tiered object to local storage for `days` days.
// It fetches the data from the remote tier and stores it as a local temporary
// copy. The tier stub metadata is preserved so a second restore (or expiry) is
// handled correctly.
func (sp *ServerPools) RestoreObject(ctx context.Context, bucket, object, versionID string, days int) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	oi, err := sp.GetObjectInfo(ctx, bucket, object, ObjectOptions{VersionID: versionID})
	if err != nil {
		return err
	}
	if oi.Tier == "" {
		return ErrNotTiered
	}
	client, err := sp.tierClient(ctx, oi.Tier)
	if err != nil {
		return err
	}

	// Fetch from remote tier.
	rc, _, err := client.Get(ctx, oi.TierKey)
	if err != nil {
		return fmt.Errorf("object: restore from tier %q: %w", oi.Tier, err)
	}
	defer rc.Close() //nolint:errcheck

	// Buffer the remote data so we can write it back as a local copy.
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, rc); err != nil {
		return fmt.Errorf("object: restore read from tier %q: %w", oi.Tier, err)
	}

	// Mark restore ongoing.
	set := sp.route(object)
	if err := set.setRestoreStatus(ctx, bucket, object, oi.VersionID, true, time.Time{}); err != nil {
		return err
	}

	// Re-write the object data to local storage under the same version.
	// We write it as a new PUT with the same user metadata and keep tier stub
	// metadata so the object's origin is recorded.
	userMeta := cloneMap(oi.UserDefined)
	userMeta[tier.MetaName] = oi.Tier
	userMeta[tier.MetaKey] = oi.TierKey
	expires := sp.now().Add(time.Duration(days) * 24 * time.Hour)
	userMeta[tier.MetaRestoreExpires] = expires.Format(time.RFC3339)
	delete(userMeta, tier.MetaRestoreOngoing)

	pr := NewPutReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	_, putErr := sp.PutObject(ctx, bucket, object, pr, ObjectOptions{
		VersionID:   oi.VersionID,
		ContentType: oi.ContentType,
		UserDefined: userMeta,
	})
	if putErr != nil {
		// Clear the ongoing flag if the restore failed.
		_ = set.setRestoreStatus(ctx, bucket, object, oi.VersionID, false, time.Time{})
		return fmt.Errorf("object: restore write: %w", putErr)
	}
	return nil
}

// setRestoreStatus writes the restore-ongoing / restore-expires metadata onto
// the live version in obj.meta without touching the object data.
func (s *erasureSet) setRestoreStatus(ctx context.Context, bucket, object, versionID string, ongoing bool, expires time.Time) error {
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		return ErrObjectNotFound
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return ErrObjectNotFound
	}
	pinned := fi.VersionID
	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		versions := metas[i]
		if versions == nil {
			return struct{}{}, nil
		}
		for j := range versions {
			if versions[j].VersionID != pinned {
				continue
			}
			if versions[j].Metadata == nil {
				versions[j].Metadata = map[string]string{}
			}
			if ongoing {
				versions[j].Metadata[tier.MetaRestoreOngoing] = "true"
				delete(versions[j].Metadata, tier.MetaRestoreExpires)
			} else {
				delete(versions[j].Metadata, tier.MetaRestoreOngoing)
				if !expires.IsZero() {
					versions[j].Metadata[tier.MetaRestoreExpires] = expires.Format(time.RFC3339)
				}
			}
			raw, err := meta.Marshal(versions)
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, object, raw)
		}
		return struct{}{}, nil
	})
	if countOK(writes) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

// --- transparent GET for tiered objects ---------------------------------

// getObjectTiered fetches the tiered object data from the remote tier client
// and returns a GetObjectReader wrapping the remote response body.
func (sp *ServerPools) getObjectTiered(ctx context.Context, oi ObjectInfo, opts ObjectOptions) (*GetObjectReader, error) {
	client, err := sp.tierClient(ctx, oi.Tier)
	if err != nil {
		return nil, err
	}
	rc, size, err := client.Get(ctx, oi.TierKey)
	if err != nil {
		return nil, fmt.Errorf("object: fetch from tier %q: %w", oi.Tier, err)
	}
	// Apply range if requested.
	var body io.Reader = rc
	if opts.Range != nil {
		offset, length, rErr := opts.Range.GetOffsetLength(oi.Size)
		if rErr != nil {
			_ = rc.Close()
			return nil, rErr
		}
		if offset > 0 {
			if _, err := io.CopyN(io.Discard, rc, offset); err != nil {
				_ = rc.Close()
				return nil, err
			}
		}
		body = io.LimitReader(rc, length)
		size = length
	}
	_ = size // size may be used for Content-Length by caller via ObjectInfo.Size
	return &GetObjectReader{
		ObjectInfo: oi,
		r:          body,
		closer:     rc.Close,
	}, nil
}

// --- helpers ------------------------------------------------------------

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	maps.Copy(out, m)
	return out
}
