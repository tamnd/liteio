// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"bytes"
	"testing"
	"time"
)

func TestMarshalUploadRoundTrip(t *testing.T) {
	in := MultipartInfo{
		UploadID:  "u-123",
		Bucket:    "b",
		Object:    "dir/key",
		Initiated: time.Unix(1700000000, 0).UTC(),
		Metadata:  map[string]string{"content-type": "application/zip"},
		Erasure: ErasureInfo{
			Algorithm:    "reedsolomon",
			DataBlocks:   4,
			ParityBlocks: 2,
			Distribution: []int{2, 0, 1, 3, 5, 4},
		},
	}
	raw, err := MarshalUpload(in)
	if err != nil {
		t.Fatalf("MarshalUpload: %v", err)
	}
	if string(raw[:magicSize]) != Magic {
		t.Fatalf("upload meta missing magic header")
	}
	out, err := UnmarshalUpload(raw)
	if err != nil {
		t.Fatalf("UnmarshalUpload: %v", err)
	}
	if out.UploadID != in.UploadID || out.Object != in.Object || !out.Initiated.Equal(in.Initiated) {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
	if out.Erasure.DataBlocks != 4 || len(out.Erasure.Distribution) != 6 {
		t.Fatalf("erasure round trip = %+v", out.Erasure)
	}
	if out.Metadata["content-type"] != "application/zip" {
		t.Fatalf("metadata round trip = %+v", out.Metadata)
	}
}

func TestMarshalPartRoundTrip(t *testing.T) {
	in := MultipartPart{
		Number:    7,
		Size:      5 << 20,
		ETag:      "9f86d081884c7d659a2feaa0c55ad015",
		ModTime:   time.Unix(1700000123, 0).UTC(),
		Checksums: [][]byte{[]byte("ck0"), []byte("ck1"), []byte("ck2")},
	}
	raw, err := MarshalPart(in)
	if err != nil {
		t.Fatalf("MarshalPart: %v", err)
	}
	out, err := UnmarshalPart(raw)
	if err != nil {
		t.Fatalf("UnmarshalPart: %v", err)
	}
	if out.Number != in.Number || out.Size != in.Size || out.ETag != in.ETag {
		t.Fatalf("round trip = %+v, want %+v", out, in)
	}
	if len(out.Checksums) != 3 || !bytes.Equal(out.Checksums[1], []byte("ck1")) {
		t.Fatalf("checksums round trip = %v", out.Checksums)
	}
}

func TestUnmarshalUploadRejectsBadMagic(t *testing.T) {
	if _, err := UnmarshalUpload([]byte("XXXX\x01\x00\x00\x00")); err != ErrBadMagic {
		t.Fatalf("bad magic = %v, want ErrBadMagic", err)
	}
	if _, err := UnmarshalPart([]byte("short")); err != ErrTruncated {
		t.Fatalf("truncated = %v, want ErrTruncated", err)
	}
}
