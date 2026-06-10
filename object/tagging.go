// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"encoding/json"
	"net/url"
	"path"
	"strings"

	"github.com/tamnd/liteio/object/meta"
)

// taggingKey is the UserDefined / FileInfo.Metadata key under which object tags
// are stored, as a URL-encoded key=value string (e.g. "env=prod&tier=hot"). The
// encoding matches what the S3 x-amz-tagging header carries, so tags written on a
// PutObject header survive a tagging GetObject round-trip without re-encoding.
const taggingKey = "x-amz-tagging"

// EncodeTags encodes a tag map into a URL-encoded string suitable for storage
// under taggingKey. An empty map encodes to the empty string (cleared tags).
func EncodeTags(tags map[string]string) string {
	if len(tags) == 0 {
		return ""
	}
	vals := url.Values{}
	for k, v := range tags {
		vals.Set(k, v)
	}
	return vals.Encode()
}

// DecodeTags parses a URL-encoded tag string back into a map. An empty string
// returns an empty map. A malformed string returns an error.
func DecodeTags(s string) (map[string]string, error) {
	if s == "" {
		return map[string]string{}, nil
	}
	vals, err := url.ParseQuery(s)
	if err != nil {
		return nil, err
	}
	m := make(map[string]string, len(vals))
	for k, vs := range vals {
		if len(vs) > 0 {
			m[k] = vs[0]
		}
	}
	return m, nil
}

// --- object tags ---------------------------------------------------------

// SetObjectTags implements ObjectLayer: it updates the tags on an object version
// without rewriting its data. It reads the current metadata quorum, replaces the
// taggingKey field, and rewrites the metadata on every drive. On a versioned
// bucket the versionID selects the target version; empty means the latest.
func (sp *ServerPools) SetObjectTags(ctx context.Context, bucket, object, versionID string, tags map[string]string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	return sp.route(object).setObjectTags(ctx, bucket, object, versionID, tags)
}

// GetObjectTags implements ObjectLayer: it returns the tags on an object version.
// On a versioned bucket the versionID selects the target version; empty means the
// latest. The returned map is always non-nil; an untagged object returns an empty
// map.
func (sp *ServerPools) GetObjectTags(ctx context.Context, bucket, object, versionID string) (map[string]string, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return nil, err
	}
	return sp.route(object).getObjectTags(ctx, bucket, object, versionID)
}

// DeleteObjectTags implements ObjectLayer: it removes all tags from an object
// version, leaving the object and its metadata otherwise intact.
func (sp *ServerPools) DeleteObjectTags(ctx context.Context, bucket, object, versionID string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	return sp.route(object).setObjectTags(ctx, bucket, object, versionID, nil)
}

func (s *erasureSet) getObjectTags(ctx context.Context, bucket, object, versionID string) (map[string]string, error) {
	if err := validObject(object); err != nil {
		return nil, err
	}
	metas := s.readAllMeta(ctx, bucket, object)
	selected, _, ok := meta.QuorumVersion(metas, versionID, s.readQuorum())
	if !ok {
		if !anyPresent(metas) {
			return nil, ErrObjectNotFound
		}
		return nil, ErrReadQuorum
	}
	fi, found := firstPresent(selected)
	if !found || fi.Deleted {
		return nil, ErrObjectNotFound
	}
	enc := ""
	if fi.Metadata != nil {
		enc = fi.Metadata[taggingKey]
	}
	return DecodeTags(enc)
}

// setObjectTags rewrites the tagging entry in the metadata of one object version
// across every drive: read quorum, update the taggingKey field, and fan-out write.
// Passing nil tags clears them.
func (s *erasureSet) setObjectTags(ctx context.Context, bucket, object, versionID string, tags map[string]string) error {
	if err := validObject(object); err != nil {
		return err
	}
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
	// versionID may be empty (meaning "latest"); pin the actual version so we
	// rewrite exactly the version we read, not whatever is latest at write time.
	pinned := fi.VersionID

	// Compute the updated tag string once; all drives get the same value.
	encoded := EncodeTags(tags)

	writes := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		versions := metas[i]
		if versions == nil {
			return struct{}{}, nil // drive had no copy; skip (heal will patch it)
		}
		for j := range versions {
			if versions[j].VersionID != pinned {
				continue
			}
			if versions[j].Metadata == nil {
				versions[j].Metadata = map[string]string{}
			}
			if encoded == "" {
				delete(versions[j].Metadata, taggingKey)
			} else {
				versions[j].Metadata[taggingKey] = encoded
			}
			raw, err := meta.Marshal(versions)
			if err != nil {
				return struct{}{}, err
			}
			return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, object, raw)
		}
		return struct{}{}, nil // version not on this drive; heal will fix it
	})
	if countOK(writes) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	s.invalidateObj(bucket, object)
	return nil
}

// --- bucket tags ---------------------------------------------------------

func (s *erasureSet) bucketTaggingPath() string { return path.Join(reserved, "tagging") }

// setBucketTagging persists the bucket's tag set (opaque JSON bytes) on every
// drive in the set.
func (s *erasureSet) setBucketTagging(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.bucketTaggingPath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

// bucketTagging reads the stored tag document. When no tags are set it returns
// ErrNoSuchBucketTagging.
func (s *erasureSet) bucketTagging(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.bucketTaggingPath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchBucketTagging
}

// deleteBucketTagging removes the bucket's tag document from every drive.
func (s *erasureSet) deleteBucketTagging(ctx context.Context, bucket string) error {
	if _, err := s.bucketTagging(ctx, bucket); err != nil {
		return nil // not set; idempotent
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, s.bucketTaggingPath(), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

// SetBucketTagging implements ObjectLayer: it attaches a tag set to a bucket.
// The document is a JSON-encoded map[string]string (validated by the caller).
func (sp *ServerPools) SetBucketTagging(ctx context.Context, bucket string, doc []byte) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketTagging(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketTagging implements ObjectLayer: it returns the bucket's tag document,
// or ErrNoSuchBucketTagging when none is set.
func (sp *ServerPools) GetBucketTagging(ctx context.Context, bucket string) ([]byte, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return nil, err
	}
	return sp.allSets()[0].bucketTagging(ctx, bucket)
}

// DeleteBucketTagging implements ObjectLayer: it removes all tags from a bucket.
// It is idempotent.
func (sp *ServerPools) DeleteBucketTagging(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketTagging(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

// TagsFromUserDefined extracts the decoded tag map from an object's UserDefined
// metadata. It is a convenience used by the S3 GET handler to include tags in
// the GetObject response when requested via ?tagging.
func TagsFromUserDefined(ud map[string]string) map[string]string {
	if ud == nil {
		return map[string]string{}
	}
	enc := ud[taggingKey]
	m, err := DecodeTags(enc)
	if err != nil {
		return map[string]string{}
	}
	return m
}

// bucketTagsFromJSON parses a JSON tag document into a map, the inverse of the
// JSON encoding SetBucketTagging stores.
func bucketTagsFromJSON(doc []byte) (map[string]string, error) {
	var m map[string]string
	if err := json.Unmarshal(doc, &m); err != nil {
		return nil, err
	}
	return m, nil
}

// ValidateTags checks the S3 tag limits: at most 10 tags per object, each key
// 1-128 chars, each value 0-256 chars, no empty keys, no duplicate keys. The S3
// bucket tag limit is 50 tags; callers pass the appropriate limit.
func ValidateTags(tags map[string]string, maxTags int) error {
	if len(tags) > maxTags {
		return ErrTooManyTags
	}
	for k, v := range tags {
		if k == "" {
			return ErrInvalidTag
		}
		if strings.ContainsAny(k, "\"&") || len(k) > 128 {
			return ErrInvalidTag
		}
		if len(v) > 256 {
			return ErrInvalidTag
		}
	}
	return nil
}

// BucketTagsToJSON encodes a tag map to JSON for bucket tag storage.
func BucketTagsToJSON(tags map[string]string) ([]byte, error) {
	return json.Marshal(tags)
}

// keep unexported helper accessible for tests
var _ = bucketTagsFromJSON
