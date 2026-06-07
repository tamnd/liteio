// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/tamnd/liteio/object/erasure"
	"github.com/tamnd/liteio/object/meta"
	"github.com/tamnd/liteio/object/placement"
	"github.com/tamnd/liteio/storage"
)

// DefaultInlineThreshold is the size at or below which an object's shards are
// stored inline in obj.meta rather than as separate part files (spec doc 03.4).
const DefaultInlineThreshold = 128 << 10

// metaName is the obj.meta path component within an object directory; the storage
// layer appends the filename, so the object layer passes the object key as the
// "path" and storage stores <bucket>/<key>/obj.meta.
//
// reserved is the per-bucket system prefix for liteio's own bookkeeping
// (versioning config, multipart staging). Object keys under it are rejected.
const reserved = ".liteio.sys"

// erasureSet is one erasure set: a fixed list of drives plus the erasure profile
// and the deployment salt that makes placement deterministic. It owns the data
// path for the objects that hash to it.
type erasureSet struct {
	drives       []storage.StorageAPI
	parity       int // M
	deploymentID [16]byte
	inlineMax    int64
	clock        func() time.Time
}

// newSet builds an erasure set over drives with M parity shards. K = N - M.
func newSet(drives []storage.StorageAPI, parity int, deploymentID [16]byte) (*erasureSet, error) {
	n := len(drives)
	if n < 2 {
		return nil, fmt.Errorf("%w: a set needs at least 2 drives, got %d", ErrInvalidArgument, n)
	}
	if parity < 1 || parity >= n {
		return nil, fmt.Errorf("%w: parity %d out of range for %d drives", ErrInvalidArgument, parity, n)
	}
	return &erasureSet{
		drives:       drives,
		parity:       parity,
		deploymentID: deploymentID,
		inlineMax:    DefaultInlineThreshold,
		clock:        time.Now,
	}, nil
}

func (s *erasureSet) dataShards() int   { return len(s.drives) - s.parity }
func (s *erasureSet) parityShards() int { return s.parity }
func (s *erasureSet) readQuorum() int   { return s.dataShards() }

// writeQuorum is K+1 so a later read quorum (K) can never observe a version that
// did not reach a majority of the data-bearing drives (spec doc 03.6).
func (s *erasureSet) writeQuorum() int {
	q := s.dataShards() + 1
	if q > len(s.drives) {
		q = len(s.drives)
	}
	return q
}

func (s *erasureSet) now() time.Time { return s.clock().UTC() }

// --- buckets -------------------------------------------------------------

func (s *erasureSet) makeBucket(ctx context.Context, bucket string, versioned bool) error {
	if err := validBucket(bucket); err != nil {
		return err
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].MakeVol(ctx, bucket)
	})
	exists := 0
	made := 0
	for _, r := range res {
		switch {
		case r.err == nil:
			made++
		case errors.Is(r.err, storage.ErrVolumeExists):
			exists++
		}
	}
	// If every reachable drive already had it, the bucket exists.
	if made == 0 && exists > 0 {
		return ErrBucketExists
	}
	if made+exists < s.writeQuorum() {
		return ErrWriteQuorum
	}
	if versioned {
		if err := s.setVersioning(ctx, bucket, true); err != nil {
			return err
		}
	}
	return nil
}

func (s *erasureSet) getBucketInfo(ctx context.Context, bucket string) (BucketInfo, error) {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (storage.VolInfo, error) {
		return s.drives[i].StatVol(ctx, bucket)
	})
	if countOK(res) < s.readQuorum() {
		return BucketInfo{}, ErrBucketNotFound
	}
	for _, r := range res {
		if r.err == nil {
			return BucketInfo{Name: bucket, Created: r.val.Created}, nil
		}
	}
	return BucketInfo{}, ErrBucketNotFound
}

func (s *erasureSet) listBuckets(ctx context.Context) ([]BucketInfo, error) {
	// Read from the first online drive; a fuller union/heal pass is a later
	// milestone. Reserved system volumes are hidden.
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		vols, err := d.ListVols(ctx)
		if err != nil {
			continue
		}
		out := make([]BucketInfo, 0, len(vols))
		for _, v := range vols {
			if strings.HasPrefix(v.Name, ".") {
				continue
			}
			out = append(out, BucketInfo{Name: v.Name, Created: v.Created})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out, nil
	}
	return nil, ErrReadQuorum
}

func (s *erasureSet) deleteBucket(ctx context.Context, bucket string, force bool) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].DeleteVol(ctx, bucket, force)
	})
	notEmpty := false
	ok := 0
	for _, r := range res {
		switch {
		case r.err == nil:
			ok++
		case errors.Is(r.err, storage.ErrVolumeNotEmpty):
			notEmpty = true
		}
	}
	if notEmpty && !force {
		return ErrBucketNotEmpty
	}
	if ok < s.writeQuorum() {
		return ErrBucketNotFound
	}
	return nil
}

// --- versioning config ---------------------------------------------------

func (s *erasureSet) versioningPath() string { return path.Join(reserved, "versioning") }

func (s *erasureSet) setVersioning(ctx context.Context, bucket string, enabled bool) error {
	state := "disabled"
	if enabled {
		state = "enabled"
	}
	return s.setVersioningState(ctx, bucket, state)
}

// setVersioningState persists the bucket's versioning state ("enabled",
// "suspended", or "disabled") on every drive in the set.
func (s *erasureSet) setVersioningState(ctx context.Context, bucket, state string) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.versioningPath(), []byte(state))
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

// versioningState returns the stored state: "enabled", "suspended", or "" when
// versioning was never configured on the bucket.
func (s *erasureSet) versioningState(ctx context.Context, bucket string) string {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.versioningPath())
		if err != nil {
			continue
		}
		switch string(data) {
		case "enabled":
			return "enabled"
		case "suspended":
			return "suspended"
		}
		return ""
	}
	return ""
}

func (s *erasureSet) versioned(ctx context.Context, bucket string) bool {
	return s.versioningState(ctx, bucket) == "enabled"
}

// --- write path ----------------------------------------------------------

func (s *erasureSet) putObject(ctx context.Context, bucket, object string, r *PutReader, opts ObjectOptions) (ObjectInfo, error) {
	if err := validBucket(bucket); err != nil {
		return ObjectInfo{}, err
	}
	if err := validObject(object); err != nil {
		return ObjectInfo{}, err
	}
	if _, err := s.getBucketInfo(ctx, bucket); err != nil {
		return ObjectInfo{}, err
	}

	// Buffer the body. Streaming block-by-block encode is a documented later
	// optimization (spec doc 06); correctness comes first.
	data, err := io.ReadAll(r.Reader)
	if err != nil {
		return ObjectInfo{}, fmt.Errorf("object: read body: %w", err)
	}

	coder, err := erasure.NewCoder(s.dataShards(), s.parityShards())
	if err != nil {
		return ObjectInfo{}, err
	}
	encoded, err := erasure.EncodeData(coder, data)
	if err != nil {
		return ObjectInfo{}, err
	}
	// EncodeData may alias one backing buffer; copy each shard so per-drive
	// writes and checksums are independent.
	shards := make([][]byte, len(encoded))
	checksums := make([][]byte, len(encoded))
	for i, sh := range encoded {
		shards[i] = append([]byte(nil), sh...)
		checksums[i] = erasure.HashShard(shards[i])
	}

	sum := md5.Sum(data)
	etag := hex.EncodeToString(sum[:])
	modTime := s.now()

	versionID := ""
	if s.versioned(ctx, bucket) {
		versionID, err = newVersionID()
		if err != nil {
			return ObjectInfo{}, err
		}
	}

	inline := int64(len(data)) <= s.inlineMax
	dist := placement.DriveOrder(object, s.deploymentID, len(s.drives))
	userMeta := buildUserMeta(opts)

	// Persist existing versions per drive so we can append the new one.
	existing := s.readAllMeta(ctx, bucket, object)

	vdir := versionDir(versionID)
	staging := path.Join(reserved, "tmp", etag+"-"+fmt.Sprint(modTime.UnixNano()))

	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, li int) (struct{}, error) {
		d := s.drives[dist[li]]
		fi := meta.FileInfo{
			Volume:    bucket,
			Name:      object,
			VersionID: versionID,
			ModTime:   modTime,
			Size:      int64(len(data)),
			ETag:      etag,
			Metadata:  userMeta,
			Erasure: meta.ErasureInfo{
				Algorithm:    erasure.Algorithm,
				DataBlocks:   s.dataShards(),
				ParityBlocks: s.parityShards(),
				BlockSize:    int64(len(data)),
				Index:        li,
				Distribution: dist,
			},
			Parts: []meta.ObjectPartInfo{{
				Number:    1,
				Size:      int64(len(data)),
				ETag:      etag,
				Checksums: [][]byte{checksums[li]},
			}},
		}
		if inline {
			fi.InlineData = shards[li]
		} else {
			// Stage this drive's shard, then commit it into the version dir.
			stagePart := path.Join(staging, "part.1")
			if err := d.CreateFile(ctx, bucket, stagePart, int64(len(shards[li])), bytes.NewReader(shards[li])); err != nil {
				return struct{}{}, err
			}
			if err := d.RenameData(ctx, bucket, staging, path.Join(object, vdir)); err != nil {
				return struct{}{}, err
			}
		}

		versions := meta.AddObjectVersion(existing[dist[li]], fi, versionID != "")
		raw, err := meta.Marshal(versions)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, d.WriteMeta(ctx, bucket, object, raw)
	})

	if countOK(writes) < s.writeQuorum() {
		return ObjectInfo{}, fmt.Errorf("%w: %d/%d drives", ErrWriteQuorum, countOK(writes), len(s.drives))
	}

	return ObjectInfo{
		Bucket:      bucket,
		Name:        object,
		VersionID:   versionID,
		IsLatest:    true,
		Size:        int64(len(data)),
		ModTime:     modTime,
		ETag:        etag,
		ContentType: userMeta["content-type"],
		UserDefined: userMeta,
	}, nil
}

// readAllMeta reads and decodes obj.meta from every drive, returning a per-drive
// version list (nil for drives that are down or have no such object).
func (s *erasureSet) readAllMeta(ctx context.Context, bucket, object string) [][]meta.FileInfo {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) ([]meta.FileInfo, error) {
		raw, err := s.drives[i].ReadMeta(ctx, bucket, object)
		if err != nil {
			return nil, err
		}
		return meta.Unmarshal(raw)
	})
	out := make([][]meta.FileInfo, len(s.drives))
	for i, r := range res {
		if r.err == nil {
			out[i] = r.val
		}
	}
	return out
}

// --- read path -----------------------------------------------------------

func (s *erasureSet) getObjectInfo(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error) {
	if err := validObject(object); err != nil {
		return ObjectInfo{}, err
	}
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, opts.VersionID, s.readQuorum())
	if !ok {
		// Distinguish "no such object" from "object exists but quorum lost".
		if !anyPresent(metas) {
			return ObjectInfo{}, ErrObjectNotFound
		}
		return ObjectInfo{}, ErrReadQuorum
	}
	fi, found := firstPresent(selected)
	if !found {
		return ObjectInfo{}, ErrObjectNotFound
	}
	return toObjectInfo(bucket, object, fi), nil
}

func (s *erasureSet) getObject(ctx context.Context, bucket, object string, opts ObjectOptions) (*GetObjectReader, error) {
	if err := validObject(object); err != nil {
		return nil, err
	}
	metas := s.readAllMeta(ctx, bucket, object)
	selected, present, ok := meta.QuorumVersion(metas, opts.VersionID, s.readQuorum())
	if !ok {
		if !anyPresent(metas) {
			return nil, ErrObjectNotFound
		}
		return nil, ErrReadQuorum
	}
	rep, found := firstPresent(selected)
	if !found {
		return nil, ErrObjectNotFound
	}
	if rep.Deleted {
		return nil, ErrObjectNotFound
	}

	k := rep.Erasure.DataBlocks
	m := rep.Erasure.ParityBlocks
	n := k + m
	coder, err := erasure.NewCoder(k, m)
	if err != nil {
		return nil, err
	}

	// Decode each part independently and concatenate. A single PUT (inline or
	// out-of-line) has exactly one part; a multipart object has one per uploaded
	// part, each its own erasure stripe.
	data := make([]byte, 0, rep.Size)
	for pi, part := range rep.Parts {
		shards := make([][]byte, n)
		for d := range s.drives {
			if !present[d] {
				continue
			}
			fi := selected[d]
			li := fi.Erasure.Index
			if li < 0 || li >= n {
				continue
			}
			shard, err := s.readPartShard(ctx, d, bucket, object, fi, part.Number)
			if err != nil {
				continue // treated as missing; reconstruction covers it
			}
			if pi < len(fi.Parts) && len(fi.Parts[pi].Checksums) == 1 {
				if !erasure.VerifyShard(shard, fi.Parts[pi].Checksums[0]) {
					continue // bitrot: drop this shard
				}
			}
			shards[li] = shard
		}
		partData, err := erasure.DecodeData(coder, shards, int(part.Size))
		if err != nil {
			return nil, ErrReadQuorum
		}
		data = append(data, partData...)
	}

	// A ranged read yields only the requested window; ObjectInfo still reports
	// the full object so the front door can emit Content-Range against the total.
	if opts.Range != nil {
		start, length, rerr := opts.Range.GetOffsetLength(rep.Size)
		if rerr != nil {
			return nil, rerr
		}
		data = data[start : start+length]
	}

	return &GetObjectReader{
		ObjectInfo: toObjectInfo(bucket, object, rep),
		r:          bytes.NewReader(data),
	}, nil
}

// readPartShard returns drive d's shard bytes for one part of version fi, inline
// or from the part file in the version directory. Inline data only ever backs a
// single-part object, so the part number is irrelevant there.
func (s *erasureSet) readPartShard(ctx context.Context, d int, bucket, object string, fi meta.FileInfo, partNumber int) ([]byte, error) {
	if fi.IsInline() {
		return fi.InlineData, nil
	}
	partPath := path.Join(object, versionDir(fi.VersionID), partFile(partNumber))
	rc, err := s.drives[d].ReadFileStream(ctx, bucket, partPath, 0, -1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// --- delete path ---------------------------------------------------------

func (s *erasureSet) deleteObject(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error) {
	if err := validObject(object); err != nil {
		return ObjectInfo{}, err
	}
	existing := s.readAllMeta(ctx, bucket, object)
	if !anyPresent(existing) {
		return ObjectInfo{}, ErrObjectNotFound
	}

	versioned := s.versioned(ctx, bucket)
	modTime := s.now()

	// Versioned delete without an explicit version: write a delete marker.
	if versioned && opts.VersionID == "" {
		markerID, err := newVersionID()
		if err != nil {
			return ObjectInfo{}, err
		}
		writes := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
			if existing[i] == nil {
				return struct{}{}, storage.ErrDriveOffline
			}
			versions := meta.AddDeleteMarker(existing[i], markerID, modTime)
			raw, err := meta.Marshal(versions)
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, object, raw)
		})
		if countOK(writes) < s.writeQuorum() {
			return ObjectInfo{}, ErrWriteQuorum
		}
		return ObjectInfo{Bucket: bucket, Name: object, VersionID: markerID, DeleteMarker: true, ModTime: modTime}, nil
	}

	// Otherwise remove a specific version (or the null version) and reclaim its
	// data. When no versions remain, the whole object directory is removed.
	targetID := opts.VersionID
	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		if existing[i] == nil {
			return struct{}{}, storage.ErrDriveOffline
		}
		versions, _ := meta.RemoveVersion(existing[i], targetID)
		// Reclaim the version's data directory (best effort).
		_ = s.drives[i].Delete(ctx, bucket, path.Join(object, versionDir(targetID)), true)
		if len(versions) == 0 {
			return struct{}{}, s.drives[i].Delete(ctx, bucket, object, true)
		}
		raw, err := meta.Marshal(versions)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, object, raw)
	})
	if countOK(writes) < s.writeQuorum() {
		return ObjectInfo{}, ErrWriteQuorum
	}
	return ObjectInfo{Bucket: bucket, Name: object, VersionID: targetID}, nil
}

// --- listing -------------------------------------------------------------

// applyListing turns a flat key list into an S3 ListObjectsV2 result: prefix
// filtering, delimiter rollup into common prefixes, start-after/continuation
// paging, and maxKeys truncation. getInfo resolves a key to its ObjectInfo and
// is used to skip delete markers and unreadable keys.
func applyListing(keys []string, prefix, token, startAfter, delim string, maxKeys int, getInfo func(key string) (ObjectInfo, error)) ListObjectsV2Info {
	sort.Strings(keys)
	after := startAfter
	if token != "" {
		after = token
	}
	var res ListObjectsV2Info
	seenPrefix := map[string]bool{}
	for _, key := range keys {
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
		oi, err := getInfo(key)
		if err != nil || oi.DeleteMarker {
			continue
		}
		res.Objects = append(res.Objects, oi)
	}
	return res
}

// walkObjects returns every object key in a bucket by descending the directory
// tree of the first online drive; a directory that directly contains obj.meta is
// an object. A union across drives and pagination at the storage layer are later
// optimizations.
func (s *erasureSet) walkObjects(ctx context.Context, bucket string) ([]string, error) {
	var drive storage.StorageAPI
	for _, d := range s.drives {
		if d.IsOnline() {
			drive = d
			break
		}
	}
	if drive == nil {
		return nil, ErrReadQuorum
	}
	var keys []string
	var walk func(dir string) error
	walk = func(dir string) error {
		entries, err := drive.ListDir(ctx, bucket, dir, -1)
		if err != nil {
			return nil // missing subtree is simply empty
		}
		hasMeta := false
		for _, e := range entries {
			if e == "obj.meta" {
				hasMeta = true
				break
			}
		}
		if hasMeta && dir != "" {
			keys = append(keys, dir)
		}
		for _, e := range entries {
			if !strings.HasSuffix(e, "/") {
				continue
			}
			name := strings.TrimSuffix(e, "/")
			child := name
			if dir != "" {
				child = dir + "/" + name
			}
			// Skip the version directories of the current object and reserved sys.
			if hasMeta && looksLikeVersionDir(name) {
				continue
			}
			if dir == "" && name == reserved {
				continue
			}
			if err := walk(child); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(""); err != nil {
		return nil, err
	}
	return keys, nil
}

// --- helpers -------------------------------------------------------------

func versionDir(versionID string) string {
	if versionID == "" {
		return "null"
	}
	return versionID
}

// looksLikeVersionDir reports whether a directory name is an object version
// directory ("null" or a UUID), so the walker does not treat it as a child key.
func looksLikeVersionDir(name string) bool {
	if name == "null" {
		return true
	}
	return len(name) == 36 && strings.Count(name, "-") == 4
}

func buildUserMeta(opts ObjectOptions) map[string]string {
	m := map[string]string{}
	for k, v := range opts.UserDefined {
		m[strings.ToLower(k)] = v
	}
	if opts.ContentType != "" {
		m["content-type"] = opts.ContentType
	}
	if _, ok := m["content-type"]; !ok {
		m["content-type"] = "application/octet-stream"
	}
	return m
}

func anyPresent(metas [][]meta.FileInfo) bool {
	for _, m := range metas {
		if len(m) > 0 {
			return true
		}
	}
	return false
}

func firstPresent(selected []meta.FileInfo) (meta.FileInfo, bool) {
	for _, fi := range selected {
		if !fi.ModTime.IsZero() || fi.VersionID != "" || fi.Deleted || fi.Size > 0 || fi.ETag != "" {
			return fi, true
		}
	}
	return meta.FileInfo{}, false
}

func validBucket(bucket string) error {
	if bucket == "" || strings.ContainsAny(bucket, `/\`) || strings.HasPrefix(bucket, ".") {
		return fmt.Errorf("%w: bucket name %q", ErrInvalidArgument, bucket)
	}
	return nil
}

func validObject(object string) error {
	if object == "" || strings.HasPrefix(object, reserved) || strings.HasPrefix(object, "/") {
		return fmt.Errorf("%w: object key %q", ErrInvalidArgument, object)
	}
	return nil
}

// count returns how many result rows (objects + common prefixes) have been
// emitted so far, for maxKeys accounting.
func (r *ListObjectsV2Info) count() int { return len(r.Objects) + len(r.Prefixes) }

// lastKey returns the continuation marker after the last emitted row.
func lastKey(r ListObjectsV2Info) string {
	if len(r.Prefixes) > 0 && (len(r.Objects) == 0 || r.Prefixes[len(r.Prefixes)-1] > r.Objects[len(r.Objects)-1].Name) {
		return r.Prefixes[len(r.Prefixes)-1]
	}
	if len(r.Objects) > 0 {
		return r.Objects[len(r.Objects)-1].Name
	}
	return ""
}
