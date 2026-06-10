// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"

	"github.com/tamnd/liteio/object/erasure"
	"github.com/tamnd/liteio/object/index"
	"github.com/tamnd/liteio/object/meta"
	"github.com/tamnd/liteio/object/placement"
	"github.com/tamnd/liteio/storage"
)

// Multipart limits from the S3 contract (spec doc 02.4).
const (
	// MinPartSize is the minimum size of every part except the last.
	MinPartSize = 5 << 20
	// MaxParts is the most parts one multipart upload may have.
	MaxParts = 10000
)

// Staging layout within a bucket volume (spec doc 03.9):
//
//	.liteio.sys/multipart/<upload-id>/
//	    obj.meta          the static MultipartInfo (written once)
//	    part.<N>          this drive's erasure shard for part N
//	    part.<N>.meta     the part record (size, ETag, all-drive checksums)
//
// upload.meta is immutable after creation and each part records itself in its own
// files, so concurrent UploadPart calls for different part numbers never contend.
func multipartBase() string      { return path.Join(reserved, "multipart") }
func uploadDir(id string) string { return path.Join(multipartBase(), id) }
func partFile(n int) string      { return "part." + strconv.Itoa(n) }
func partMetaFile(n int) string  { return "part." + strconv.Itoa(n) + ".meta" }

// --- create --------------------------------------------------------------

func (s *erasureSet) newMultipartUpload(ctx context.Context, bucket, object string, opts ObjectOptions) (string, error) {
	if err := validBucket(bucket); err != nil {
		return "", err
	}
	if err := validObject(object); err != nil {
		return "", err
	}
	if _, err := s.getBucketInfo(ctx, bucket); err != nil {
		return "", err
	}

	uploadID, err := newVersionID()
	if err != nil {
		return "", err
	}
	info := meta.MultipartInfo{
		UploadID:  uploadID,
		Bucket:    bucket,
		Object:    object,
		Initiated: s.now(),
		Metadata:  buildUserMeta(opts),
		Erasure: meta.ErasureInfo{
			Algorithm:    erasure.Algorithm,
			DataBlocks:   s.dataShards(),
			ParityBlocks: s.parityShards(),
			Distribution: placement.DriveOrder(object, s.deploymentID, len(s.drives)),
		},
	}
	raw, err := meta.MarshalUpload(info)
	if err != nil {
		return "", err
	}
	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, uploadDir(uploadID), raw)
	})
	if countOK(writes) < s.writeQuorum() {
		return "", ErrWriteQuorum
	}
	return uploadID, nil
}

// readUploadInfo returns the static upload record, or ErrNoSuchUpload.
func (s *erasureSet) readUploadInfo(ctx context.Context, bucket, uploadID string) (meta.MultipartInfo, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		raw, err := d.ReadMeta(ctx, bucket, uploadDir(uploadID))
		if err != nil {
			continue
		}
		return meta.UnmarshalUpload(raw)
	}
	return meta.MultipartInfo{}, ErrNoSuchUpload
}

// --- upload part ---------------------------------------------------------

func (s *erasureSet) putObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, r *PutReader, _ ObjectOptions) (PartInfo, error) {
	if partID < 1 || partID > MaxParts {
		return PartInfo{}, fmt.Errorf("%w: part number %d", ErrInvalidArgument, partID)
	}
	info, err := s.readUploadInfo(ctx, bucket, uploadID)
	if err != nil {
		return PartInfo{}, err
	}
	if info.Object != object {
		return PartInfo{}, ErrNoSuchUpload
	}

	data, err := io.ReadAll(r.Reader)
	if err != nil {
		return PartInfo{}, fmt.Errorf("object: read part body: %w", err)
	}

	coder, err := erasure.NewCoder(s.dataShards(), s.parityShards())
	if err != nil {
		return PartInfo{}, err
	}
	encoded, err := erasure.EncodeData(coder, data)
	if err != nil {
		return PartInfo{}, err
	}
	shards := make([][]byte, len(encoded))
	checksums := make([][]byte, len(encoded))
	for i, sh := range encoded {
		shards[i] = append([]byte(nil), sh...)
		checksums[i] = erasure.HashShard(shards[i])
	}

	sum := md5.Sum(data)
	etag := hex.EncodeToString(sum[:])
	modTime := s.now()

	partRec := meta.MultipartPart{
		Number:    partID,
		Size:      int64(len(data)),
		ETag:      etag,
		ModTime:   modTime,
		Checksums: checksums,
	}
	recBytes, err := meta.MarshalPart(partRec)
	if err != nil {
		return PartInfo{}, err
	}

	dist := info.Erasure.Distribution
	dir := uploadDir(uploadID)
	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, li int) (struct{}, error) {
		d := s.drives[dist[li]]
		if err := d.CreateFile(ctx, bucket, path.Join(dir, partFile(partID)), int64(len(shards[li])), bytes.NewReader(shards[li])); err != nil {
			return struct{}{}, err
		}
		return struct{}{}, d.CreateFile(ctx, bucket, path.Join(dir, partMetaFile(partID)), int64(len(recBytes)), bytes.NewReader(recBytes))
	})
	if countOK(writes) < s.writeQuorum() {
		return PartInfo{}, ErrWriteQuorum
	}
	return PartInfo{PartNumber: partID, LastModified: modTime, ETag: etag, Size: int64(len(data))}, nil
}

// readPartRecord reads one part's record from the first drive that has it.
func (s *erasureSet) readPartRecord(ctx context.Context, bucket, uploadID string, partID int) (meta.MultipartPart, bool) {
	for li := range s.drives {
		d := s.drives[li]
		if !d.IsOnline() {
			continue
		}
		raw, err := readWhole(ctx, d, bucket, path.Join(uploadDir(uploadID), partMetaFile(partID)))
		if err != nil {
			continue
		}
		rec, err := meta.UnmarshalPart(raw)
		if err != nil {
			continue
		}
		return rec, true
	}
	return meta.MultipartPart{}, false
}

// listPartRecords returns every uploaded part record, sorted by part number.
func (s *erasureSet) listPartRecords(ctx context.Context, bucket, uploadID string) ([]meta.MultipartPart, error) {
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
	entries, err := drive.ListDir(ctx, bucket, uploadDir(uploadID), -1)
	if err != nil {
		return nil, nil
	}
	var parts []meta.MultipartPart
	for _, e := range entries {
		n, ok := parsePartMeta(e)
		if !ok {
			continue
		}
		if rec, ok := s.readPartRecord(ctx, bucket, uploadID, n); ok {
			parts = append(parts, rec)
		}
	}
	sort.Slice(parts, func(i, j int) bool { return parts[i].Number < parts[j].Number })
	return parts, nil
}

// --- complete ------------------------------------------------------------

func (s *erasureSet) completeMultipartUpload(ctx context.Context, bucket, object, uploadID string, parts []CompletePart, _ ObjectOptions) (ObjectInfo, error) {
	info, err := s.readUploadInfo(ctx, bucket, uploadID)
	if err != nil {
		return ObjectInfo{}, err
	}
	if info.Object != object {
		return ObjectInfo{}, ErrNoSuchUpload
	}
	if len(parts) == 0 {
		return ObjectInfo{}, fmt.Errorf("%w: no parts supplied", ErrInvalidPart)
	}

	// Part numbers must be strictly ascending; validate the whole list before
	// touching any data so an ordering error is never masked by a size error.
	for i := 1; i < len(parts); i++ {
		if parts[i].PartNumber <= parts[i-1].PartNumber {
			return ObjectInfo{}, ErrInvalidPartOrder
		}
	}

	// Resolve each part and enforce the 5 MiB minimum on every part but the last.
	resolved := make([]meta.MultipartPart, len(parts))
	var totalSize int64
	md5buf := make([]byte, 0, len(parts)*md5.Size)
	for i, cp := range parts {
		rec, ok := s.readPartRecord(ctx, bucket, uploadID, cp.PartNumber)
		if !ok || rec.ETag != normalizeETag(cp.ETag) {
			return ObjectInfo{}, fmt.Errorf("%w: part %d", ErrInvalidPart, cp.PartNumber)
		}
		if i < len(parts)-1 && rec.Size < MinPartSize {
			return ObjectInfo{}, fmt.Errorf("%w: part %d is %d bytes", ErrEntityTooSmall, cp.PartNumber, rec.Size)
		}
		raw, derr := hex.DecodeString(rec.ETag)
		if derr != nil {
			return ObjectInfo{}, fmt.Errorf("%w: part %d etag", ErrInvalidPart, cp.PartNumber)
		}
		md5buf = append(md5buf, raw...)
		totalSize += rec.Size
		resolved[i] = rec
	}
	compositeSum := md5.Sum(md5buf)
	etag := hex.EncodeToString(compositeSum[:]) + "-" + strconv.Itoa(len(parts))
	modTime := s.now()

	versionID := ""
	if s.versioned(ctx, bucket) {
		versionID, err = newVersionID()
		if err != nil {
			return ObjectInfo{}, err
		}
	}
	vdir := versionDir(versionID)
	existing := s.readAllMeta(ctx, bucket, object)
	dist := info.Erasure.Distribution
	dir := uploadDir(uploadID)

	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, li int) (struct{}, error) {
		d := s.drives[dist[li]]
		fiParts := make([]meta.ObjectPartInfo, len(resolved))
		for i, rec := range resolved {
			// Commit this drive's shard for the part by renaming it into the
			// version directory; obj.meta is written last as the commit point.
			if err := d.RenameFile(ctx, bucket, path.Join(dir, partFile(rec.Number)), bucket, path.Join(object, vdir, partFile(rec.Number))); err != nil {
				return struct{}{}, err
			}
			var cks [][]byte
			if li < len(rec.Checksums) {
				cks = [][]byte{rec.Checksums[li]}
			}
			fiParts[i] = meta.ObjectPartInfo{Number: rec.Number, Size: rec.Size, ETag: rec.ETag, Checksums: cks}
		}
		fi := meta.FileInfo{
			Volume:    bucket,
			Name:      object,
			VersionID: versionID,
			ModTime:   modTime,
			Size:      totalSize,
			ETag:      etag,
			Metadata:  info.Metadata,
			Erasure: meta.ErasureInfo{
				Algorithm:    erasure.Algorithm,
				DataBlocks:   info.Erasure.DataBlocks,
				ParityBlocks: info.Erasure.ParityBlocks,
				Index:        li,
				Distribution: dist,
			},
			Parts: fiParts,
		}
		versions := meta.AddObjectVersion(existing[dist[li]], fi, versionID != "")
		raw, err := meta.Marshal(versions)
		if err != nil {
			return struct{}{}, err
		}
		return struct{}{}, d.WriteMeta(ctx, bucket, object, raw)
	})
	if countOK(writes) < s.writeQuorum() {
		return ObjectInfo{}, ErrWriteQuorum
	}
	s.maybeHeal(countOK(writes), bucket, object, versionID)

	// Reclaim the staging tree (best effort; orphaned staging is harmless).
	_ = fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, dir, true)
	})

	oi := ObjectInfo{
		Bucket:      bucket,
		Name:        object,
		VersionID:   versionID,
		IsLatest:    true,
		Size:        totalSize,
		ModTime:     modTime,
		ETag:        etag,
		ContentType: info.Metadata["content-type"],
		UserDefined: info.Metadata,
	}
	if s.idx != nil {
		_ = s.idx.Put(bucket, object, index.Entry{
			ETag:        etag,
			Size:        totalSize,
			ModTime:     modTime,
			ContentType: info.Metadata["content-type"],
			VersionID:   versionID,
			UserDefined: info.Metadata,
		})
	}
	if s.mem != nil {
		s.mem.Put(bucket, object, oi)
	}
	if s.objCache != nil {
		s.objCache.set(bucket, object, oi)
	}
	return oi, nil
}

// --- abort & list --------------------------------------------------------

func (s *erasureSet) abortMultipartUpload(ctx context.Context, bucket, uploadID string) error {
	if _, err := s.readUploadInfo(ctx, bucket, uploadID); err != nil {
		return err
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, uploadDir(uploadID), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) listObjectParts(ctx context.Context, bucket, object, uploadID string, partNumberMarker, maxParts int) (ListPartsInfo, error) {
	info, err := s.readUploadInfo(ctx, bucket, uploadID)
	if err != nil {
		return ListPartsInfo{}, err
	}
	if maxParts <= 0 {
		maxParts = 1000
	}
	recs, err := s.listPartRecords(ctx, bucket, uploadID)
	if err != nil {
		return ListPartsInfo{}, err
	}
	out := ListPartsInfo{
		Bucket:           bucket,
		Object:           object,
		UploadID:         uploadID,
		PartNumberMarker: partNumberMarker,
		MaxParts:         maxParts,
		UserDefined:      info.Metadata,
	}
	for _, rec := range recs {
		if rec.Number <= partNumberMarker {
			continue
		}
		if len(out.Parts) >= maxParts {
			out.IsTruncated = true
			break
		}
		out.Parts = append(out.Parts, PartInfo{
			PartNumber:   rec.Number,
			LastModified: rec.ModTime,
			ETag:         rec.ETag,
			Size:         rec.Size,
		})
		out.NextPartNumberMarker = rec.Number
	}
	return out, nil
}

// listMultipartUploads enumerates in-progress uploads in a bucket on this set.
// Pagination across the union of sets is applied by the pools layer.
func (s *erasureSet) listMultipartUploads(ctx context.Context, bucket, prefix string) ([]MultipartUpload, error) {
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
	entries, err := drive.ListDir(ctx, bucket, multipartBase(), -1)
	if err != nil {
		return nil, nil // no staging tree yet: no uploads
	}
	var out []MultipartUpload
	for _, e := range entries {
		if !strings.HasSuffix(e, "/") {
			continue
		}
		id := strings.TrimSuffix(e, "/")
		info, err := s.readUploadInfo(ctx, bucket, id)
		if err != nil {
			continue
		}
		if prefix != "" && !strings.HasPrefix(info.Object, prefix) {
			continue
		}
		out = append(out, MultipartUpload{
			Bucket:    bucket,
			Object:    info.Object,
			UploadID:  id,
			Initiated: info.Initiated,
		})
	}
	return out, nil
}

// --- helpers -------------------------------------------------------------

// readWhole reads an entire file from a drive via its streaming reader.
func readWhole(ctx context.Context, d storage.StorageAPI, volume, p string) ([]byte, error) {
	rc, err := d.ReadFileStream(ctx, volume, p, 0, -1)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	return io.ReadAll(rc)
}

// parsePartMeta reports whether a directory entry is a part record file and, if
// so, its part number.
func parsePartMeta(entry string) (int, bool) {
	const prefix, suffix = "part.", ".meta"
	if !strings.HasPrefix(entry, prefix) || !strings.HasSuffix(entry, suffix) {
		return 0, false
	}
	num := entry[len(prefix) : len(entry)-len(suffix)]
	n, err := strconv.Atoi(num)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}

// normalizeETag strips surrounding quotes from a client-supplied part ETag so it
// compares equal to the stored hex MD5.
func normalizeETag(etag string) string {
	return strings.Trim(etag, `"`)
}
