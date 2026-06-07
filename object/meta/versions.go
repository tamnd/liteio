// SPDX-License-Identifier: Apache-2.0

package meta

import "time"

// This file holds the version-list operations that obj.meta supports: finding the
// latest or a specific version, adding object versions and delete markers, removing
// a version, and reconciling disagreeing per-drive views into the read-quorum
// truth (spec docs 07.3 and 05.4). Version lists are always ordered newest-first.

// Latest returns the most recent version entry. ok is false for an empty list.
func Latest(versions []FileInfo) (fi FileInfo, ok bool) {
	if len(versions) == 0 {
		return FileInfo{}, false
	}
	return versions[0], true
}

// FindVersion returns the entry for versionID. An empty versionID means "the
// latest version" (S3 GET without a versionId). ok is false if not found.
func FindVersion(versions []FileInfo, versionID string) (fi FileInfo, ok bool) {
	if versionID == "" {
		return Latest(versions)
	}
	for _, v := range versions {
		if v.VersionID == versionID {
			return v, true
		}
	}
	return FileInfo{}, false
}

// AddObjectVersion returns the version list with fi installed as the current
// version.
//
//   - versioned: fi is prepended as a new version; prior versions become
//     noncurrent and remain readable by versionId.
//   - unversioned/suspended: fi uses the null version ("") and overwrites any
//     existing null-version entry, while preexisting UUID versions remain.
func AddObjectVersion(versions []FileInfo, fi FileInfo, versioned bool) []FileInfo {
	if versioned {
		return prepend(versions, fi)
	}
	fi.VersionID = ""
	out := removeMatching(versions, func(v FileInfo) bool { return v.VersionID == "" })
	return prepend(out, fi)
}

// AddDeleteMarker prepends a delete marker. In a versioned bucket this hides the
// object while keeping prior versions; versionID is the marker's own version
// (empty for a suspended-bucket null marker).
func AddDeleteMarker(versions []FileInfo, versionID string, modTime time.Time) []FileInfo {
	marker := FileInfo{VersionID: versionID, Deleted: true, ModTime: modTime}
	if versionID == "" {
		versions = removeMatching(versions, func(v FileInfo) bool { return v.VersionID == "" })
	}
	return prepend(versions, marker)
}

// RemoveVersion removes the entry with versionID (the null version when empty) and
// reports whether anything was removed.
func RemoveVersion(versions []FileInfo, versionID string) ([]FileInfo, bool) {
	out := removeMatching(versions, func(v FileInfo) bool { return v.VersionID == versionID })
	return out, len(out) != len(versions)
}

// prepend installs fi at the front (newest) and recomputes IsLatest.
func prepend(versions []FileInfo, fi FileInfo) []FileInfo {
	out := make([]FileInfo, 0, len(versions)+1)
	fi.IsLatest = true
	out = append(out, fi)
	for _, v := range versions {
		if v.VersionID == fi.VersionID {
			// Replacing an existing entry with the same version id.
			continue
		}
		v.IsLatest = false
		out = append(out, v)
	}
	return out
}

func removeMatching(versions []FileInfo, match func(FileInfo) bool) []FileInfo {
	out := make([]FileInfo, 0, len(versions))
	for _, v := range versions {
		if match(v) {
			continue
		}
		out = append(out, v)
	}
	markLatest(out)
	return out
}

// markLatest sets IsLatest on the first entry and clears it on the rest.
func markLatest(versions []FileInfo) {
	for i := range versions {
		versions[i].IsLatest = i == 0
	}
}

// versionKey identifies a logical version across drives for reconciliation. The
// VersionID is the primary key; the null version is disambiguated by mod time
// (UnixNano), since unversioned overwrites all share the empty id.
type versionKey struct {
	id      string
	modNano int64
}

func keyOf(fi FileInfo) versionKey {
	k := versionKey{id: fi.VersionID}
	if fi.VersionID == "" {
		k.modNano = fi.ModTime.UnixNano()
	}
	return k
}

// QuorumVersion reconciles per-drive metadata into the read-quorum truth for the
// requested version (latest when versionID is empty).
//
// metas[i] is drive i's full version list (nil if drive i did not respond). It
// returns, for each drive index, that drive's FileInfo for the agreed version
// (the zero FileInfo with a false present flag for drives that lack it), the agreed
// version's identity, and ok=true only if at least quorum drives carry it.
//
// A drive whose view is stale (missing the agreed version) is reported as not
// present so the caller can enqueue it for heal while still serving the read.
func QuorumVersion(metas [][]FileInfo, versionID string, quorum int) (selected []FileInfo, present []bool, ok bool) {
	n := len(metas)
	selected = make([]FileInfo, n)
	present = make([]bool, n)

	// Tally candidate versions by key, tracking how many drives hold each.
	counts := map[versionKey]int{}
	repr := map[versionKey]FileInfo{}
	for _, vers := range metas {
		fi, found := FindVersion(vers, versionID)
		if !found {
			continue
		}
		k := keyOf(fi)
		counts[k]++
		if _, seen := repr[k]; !seen {
			repr[k] = fi
		}
	}
	if len(counts) == 0 {
		return selected, present, false
	}

	// Choose the version with the most agreeing drives; break ties by the newer
	// mod time so a concurrent write in progress resolves to the latest committed
	// state.
	var best versionKey
	bestCount := -1
	for k, c := range counts {
		if c > bestCount || (c == bestCount && repr[k].ModTime.After(repr[best].ModTime)) {
			best, bestCount = k, c
		}
	}
	if bestCount < quorum {
		return selected, present, false
	}

	// Fill the per-drive selection for the winning version.
	for i, vers := range metas {
		for _, v := range vers {
			if keyOf(v) == best {
				selected[i] = v
				present[i] = true
				break
			}
		}
	}
	return selected, present, true
}
