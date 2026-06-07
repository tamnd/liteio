// SPDX-License-Identifier: Apache-2.0

// Package meta owns liteio's self-describing object metadata: the obj.meta
// on-disk format and the FileInfo model it (de)serializes (spec 2020, docs 07 and
// 11.4/11.5).
//
// obj.meta lives next to an object's data on every drive of its set and contains
// everything needed to read the object back: erasure parameters, the part layout,
// sizes, checksums, the wire ETag, and user/system metadata. There is no separate
// metadata store. The file holds every version of the object, newest-first, so the
// latest version is found without scanning the whole list.
package meta

import "time"

// FileInfo is the per-drive projection of one object version (an ObjectVersion or
// a DeleteMarker in the spec's terms). A drive's obj.meta is a list of these,
// newest-first. FileInfo carries the fields that drive's shard needs plus the
// shared logical metadata (ETag, sizes, user metadata) that is identical on every
// drive.
type FileInfo struct {
	// Volume and Name are the bucket and object key. They are usually implied by
	// the obj.meta's location and may be empty on disk.
	Volume string `msgpack:"v,omitempty"`
	Name   string `msgpack:"n,omitempty"`

	// VersionID is the version UUID, or empty for the null version of an
	// unversioned/suspended bucket.
	VersionID string `msgpack:"id,omitempty"`

	// IsLatest is set on the current version. It is derived (the first non-stale
	// entry) and not authoritative on disk, but is convenient for listing.
	IsLatest bool `msgpack:"l,omitempty"`

	// Deleted marks a delete marker: the object reads as "not found" at this
	// version, but prior versions remain.
	Deleted bool `msgpack:"d,omitempty"`

	ModTime time.Time `msgpack:"mt"`

	// Size is the logical object size in bytes (what the client PUT). ActualSize is
	// the stored size after any compression (equal to Size when not compressed).
	Size       int64 `msgpack:"sz"`
	ActualSize int64 `msgpack:"asz,omitempty"`

	Erasure ErasureInfo `msgpack:"ec"`

	// Parts lists the object's parts in order. A single PUT has one part; a
	// multipart upload has one part per uploaded part.
	Parts []ObjectPartInfo `msgpack:"p,omitempty"`

	// ETag is the wire ETag returned to S3 clients: the hex MD5 for a single PUT,
	// or MD5(part-MD5s)-N for a multipart object.
	ETag string `msgpack:"et,omitempty"`

	// Checksum is the optional additional S3 checksum (CRC32/CRC32C/CRC64NVME/
	// SHA1/SHA256) the client requested on PUT.
	Checksum *Checksum `msgpack:"cs,omitempty"`

	// Metadata holds user metadata (x-amz-meta-*) and the replayed HTTP headers
	// (Content-Type, Content-Encoding, Cache-Control, ...).
	Metadata map[string]string `msgpack:"md,omitempty"`

	// InlineData, when non-nil, holds this drive's erasure shard for a small object
	// stored inline (spec doc 03.4). The object then has exactly one part and no
	// part.N file; Parts[0].Checksums[0] is the shard's bitrot checksum.
	InlineData []byte `msgpack:"inl,omitempty"`
}

// ErasureInfo records the erasure-coding parameters for a version, stored per
// object so changing the cluster default does not strand existing objects.
type ErasureInfo struct {
	Algorithm    string `msgpack:"a"`              // "reedsolomon"
	DataBlocks   int    `msgpack:"k"`              // K
	ParityBlocks int    `msgpack:"m"`              // M
	BlockSize    int64  `msgpack:"bs"`             // stripe size fed to the encoder
	Index        int    `msgpack:"i"`              // this drive's logical shard index (0-based)
	Distribution []int  `msgpack:"dist,omitempty"` // logical->physical drive order (placement.DriveOrder)
}

// ObjectPartInfo describes one part of an object on this drive.
type ObjectPartInfo struct {
	Number int    `msgpack:"num"`
	Size   int64  `msgpack:"sz"`           // logical size of this part (user bytes)
	ETag   string `msgpack:"et,omitempty"` // part MD5 (multipart completion uses this)
	// Checksums holds one HighwayHash-256 bitrot checksum per stripe of this
	// drive's shard for this part (one entry for an inline object).
	Checksums [][]byte `msgpack:"cks,omitempty"`
}

// Checksum is an additional S3 content checksum stored verbatim and replayed on
// GetObjectAttributes / GET / HEAD.
type Checksum struct {
	Algorithm string `msgpack:"a"` // CRC32, CRC32C, CRC64NVME, SHA1, SHA256
	Value     string `msgpack:"v"` // base64 as S3 reports it
}

// IsInline reports whether this version's data is stored inline in obj.meta.
func (fi FileInfo) IsInline() bool { return fi.InlineData != nil }

// DataShards returns K for this version's erasure profile.
func (fi FileInfo) DataShards() int { return fi.Erasure.DataBlocks }

// ParityShards returns M for this version's erasure profile.
func (fi FileInfo) ParityShards() int { return fi.Erasure.ParityBlocks }
