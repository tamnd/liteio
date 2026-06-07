// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"sort"
	"strings"

	"github.com/tamnd/liteio/object/meta"
)

// objectVersions returns every read-quorum version of one object on this set,
// newest-first, as ObjectInfos. Delete markers are included (DeleteMarker set) so
// ListObjectVersions can render them.
func (s *erasureSet) objectVersions(ctx context.Context, bucket, object string) []ObjectInfo {
	metas := s.readAllMeta(ctx, bucket, object)
	vers := meta.QuorumVersions(metas, s.readQuorum())
	out := make([]ObjectInfo, 0, len(vers))
	for _, fi := range vers {
		out = append(out, toObjectInfo(bucket, object, fi))
	}
	return out
}

// SetBucketVersioning implements ObjectLayer: it records the bucket's versioning
// state on every set the bucket spans. Enabled and Suspended are persisted as
// distinct states so a suspended bucket keeps its existing versions while writing
// "null" versions going forward.
func (sp *ServerPools) SetBucketVersioning(ctx context.Context, bucket string, cfg VersioningConfig) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	state := "disabled"
	switch {
	case cfg.Enabled:
		state = "enabled"
	case cfg.Suspended:
		state = "suspended"
	}
	for _, set := range sp.allSets() {
		if err := set.setVersioningState(ctx, bucket, state); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketVersioning implements ObjectLayer: it reports the bucket's versioning
// state. Every set carries the same state, so the first set is authoritative.
func (sp *ServerPools) GetBucketVersioning(ctx context.Context, bucket string) (VersioningConfig, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return VersioningConfig{}, err
	}
	switch sp.allSets()[0].versioningState(ctx, bucket) {
	case "enabled":
		return VersioningConfig{Enabled: true}, nil
	case "suspended":
		return VersioningConfig{Suspended: true}, nil
	}
	return VersioningConfig{}, nil
}

// ListObjectVersions implements ObjectLayer: it enumerates every version (and
// delete marker) of every object in a bucket, applying prefix and delimiter
// filtering and paging by the (key, version-id) cursor S3 uses.
func (sp *ServerPools) ListObjectVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker, delim string, maxKeys int) (ListObjectVersionsInfo, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return ListObjectVersionsInfo{}, err
	}
	if maxKeys <= 0 {
		maxKeys = 1000
	}

	// Gather the union of object keys across every set.
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

	versionsOf := func(key string) []ObjectInfo {
		return sp.route(key).objectVersions(ctx, bucket, key)
	}
	return applyVersionListing(keys, prefix, keyMarker, versionIDMarker, delim, maxKeys, versionsOf), nil
}

// applyVersionListing turns a sorted key list into a ListObjectVersions result.
// It rolls keys under a delimiter into common prefixes, resumes after the
// (keyMarker, versionIDMarker) cursor, and truncates at maxKeys, reporting the
// next cursor. versionsOf returns a key's versions newest-first.
func applyVersionListing(keys []string, prefix, keyMarker, versionIDMarker, delim string, maxKeys int, versionsOf func(key string) []ObjectInfo) ListObjectVersionsInfo {
	var res ListObjectVersionsInfo
	seenPrefix := map[string]bool{}
	count := 0
	// lastKey/lastVersion track the most recently emitted entry so a truncated page
	// reports a cursor that resumes *after* it (S3 skips up to and including the
	// marker on the next page).
	var lastKey, lastVersion string
	truncate := func() ListObjectVersionsInfo {
		res.IsTruncated = true
		res.NextKeyMarker = lastKey
		res.NextVersionIDMarker = lastVersion
		return res
	}
	// The cursor is passed once we move beyond keyMarker; within keyMarker itself we
	// additionally skip versions up to and including versionIDMarker.
	passedKey := keyMarker == ""

	for _, key := range keys {
		if prefix != "" && !strings.HasPrefix(key, prefix) {
			continue
		}

		// Delimiter rollup: a key with the delimiter past the prefix becomes a
		// common prefix rather than a set of version rows.
		if delim != "" {
			rest := key[len(prefix):]
			if idx := strings.Index(rest, delim); idx >= 0 {
				cp := prefix + rest[:idx+len(delim)]
				if seenPrefix[cp] {
					continue
				}
				if !passedKey {
					if cp == keyMarker {
						passedKey = true
					}
					seenPrefix[cp] = true
					continue
				}
				if count >= maxKeys {
					return truncate()
				}
				seenPrefix[cp] = true
				res.Prefixes = append(res.Prefixes, cp)
				lastKey, lastVersion = cp, ""
				count++
				continue
			}
		}

		// Resolve the cursor at key granularity.
		if !passedKey {
			if key < keyMarker {
				continue
			}
			if key > keyMarker {
				passedKey = true
			}
			// key == keyMarker: fall through, but skip versions up to the version
			// marker below.
		}

		skipToVersion := key == keyMarker && versionIDMarker != ""
		for _, oi := range versionsOf(key) {
			if skipToVersion {
				if oi.VersionID == versionIDMarker {
					skipToVersion = false
				}
				continue
			}
			if count >= maxKeys {
				return truncate()
			}
			res.Objects = append(res.Objects, oi)
			lastKey, lastVersion = key, oi.VersionID
			count++
		}
	}
	return res
}
