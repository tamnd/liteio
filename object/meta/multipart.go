// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"encoding/binary"
	"fmt"
	"time"

	"github.com/vmihailenco/msgpack/v5"
)

// MultipartInfo is the static record of an in-progress multipart upload, written
// once at NewMultipartUpload as the upload's obj.meta inside its staging tree and
// replicated identically on every drive of the set. It never changes after
// creation: each uploaded part records itself in its own per-part file, so
// concurrent UploadPart calls never contend on a shared record (spec docs 02.4
// and 03.9).
type MultipartInfo struct {
	UploadID  string            `msgpack:"uid"`
	Bucket    string            `msgpack:"bkt"`
	Object    string            `msgpack:"obj"`
	Initiated time.Time         `msgpack:"init"`
	Metadata  map[string]string `msgpack:"md,omitempty"`
	// Erasure carries the K/M geometry and the object's drive distribution so
	// every part is encoded and placed the same way; Index and BlockSize are
	// unused here (parts size independently).
	Erasure ErasureInfo `msgpack:"ec"`
}

// MultipartPart is the per-part record written alongside a part's shards at
// UploadPart and read back at completion. Checksums holds the bitrot checksum of
// every logical shard (N entries, indexed by shard index), so completion hands
// each drive its own shard's checksum without re-reading any data.
type MultipartPart struct {
	Number    int       `msgpack:"num"`
	Size      int64     `msgpack:"sz"` // logical part size (user bytes)
	ETag      string    `msgpack:"et"` // hex MD5 of the part's user bytes
	ModTime   time.Time `msgpack:"mt"`
	Checksums [][]byte  `msgpack:"cks,omitempty"`
}

// uploadBody is the MessagePack payload of an upload's obj.meta.
type uploadBody struct {
	Info MultipartInfo `msgpack:"mp"`
}

// partBody is the MessagePack payload of a part record file.
type partBody struct {
	Part MultipartPart `msgpack:"pt"`
}

// MarshalUpload encodes a MultipartInfo with the shared LIO2 header.
func MarshalUpload(info MultipartInfo) ([]byte, error) {
	return marshalWithHeader(uploadBody{Info: info}, "marshal upload")
}

// UnmarshalUpload validates the header and decodes a MultipartInfo.
func UnmarshalUpload(data []byte) (MultipartInfo, error) {
	var b uploadBody
	if err := unmarshalWithHeader(data, &b, "unmarshal upload"); err != nil {
		return MultipartInfo{}, err
	}
	return b.Info, nil
}

// MarshalPart encodes a MultipartPart with the shared LIO2 header.
func MarshalPart(p MultipartPart) ([]byte, error) {
	return marshalWithHeader(partBody{Part: p}, "marshal part")
}

// UnmarshalPart validates the header and decodes a MultipartPart.
func UnmarshalPart(data []byte) (MultipartPart, error) {
	var b partBody
	if err := unmarshalWithHeader(data, &b, "unmarshal part"); err != nil {
		return MultipartPart{}, err
	}
	return b.Part, nil
}

// marshalWithHeader writes the 8-byte LIO2 header then the MessagePack body of v.
func marshalWithHeader(v any, what string) ([]byte, error) {
	payload, err := msgpack.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("meta: %s: %w", what, err)
	}
	out := make([]byte, headerSize, headerSize+len(payload))
	copy(out, Magic)
	binary.LittleEndian.PutUint16(out[4:6], FormatMajor)
	binary.LittleEndian.PutUint16(out[6:8], FormatMinor)
	return append(out, payload...), nil
}

// unmarshalWithHeader validates the LIO2 header and decodes the body into v.
func unmarshalWithHeader(data []byte, v any, what string) error {
	if len(data) < headerSize {
		return ErrTruncated
	}
	if string(data[:magicSize]) != Magic {
		return ErrBadMagic
	}
	if major := binary.LittleEndian.Uint16(data[4:6]); major != FormatMajor {
		return fmt.Errorf("%w: %d", ErrUnsupportedMajor, major)
	}
	if err := msgpack.Unmarshal(data[headerSize:], v); err != nil {
		return fmt.Errorf("meta: %s: %w", what, err)
	}
	return nil
}
