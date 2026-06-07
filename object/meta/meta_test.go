// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"bytes"
	"testing"
	"time"
)

func sampleVersion(id string, mod time.Time) FileInfo {
	return FileInfo{
		Volume:    "photos",
		Name:      "cat.jpg",
		VersionID: id,
		ModTime:   mod,
		Size:      2048,
		ETag:      "d41d8cd98f00b204e9800998ecf8427e",
		Erasure: ErasureInfo{
			Algorithm:    "reedsolomon",
			DataBlocks:   4,
			ParityBlocks: 2,
			BlockSize:    1 << 20,
			Index:        2,
			Distribution: []int{3, 0, 5, 1, 4, 2},
		},
		Parts: []ObjectPartInfo{{
			Number:    1,
			Size:      2048,
			ETag:      "abc",
			Checksums: [][]byte{bytes.Repeat([]byte{0xab}, 32)},
		}},
		Metadata: map[string]string{
			"content-type":      "image/jpeg",
			"x-amz-meta-author": "tam",
		},
		Checksum: &Checksum{Algorithm: "CRC32C", Value: "AAAAAA=="},
	}
}

func TestMarshalRoundTrip(t *testing.T) {
	mod := time.Now().UTC().Truncate(time.Millisecond)
	in := []FileInfo{sampleVersion("v2", mod), sampleVersion("v1", mod.Add(-time.Hour))}

	raw, err := Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if string(raw[:4]) != Magic {
		t.Fatalf("magic = %q want %q", raw[:4], Magic)
	}

	out, err := Unmarshal(raw)
	if err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("got %d versions want 2", len(out))
	}
	if out[0].VersionID != "v2" || out[1].VersionID != "v1" {
		t.Fatalf("order not preserved: %v", []string{out[0].VersionID, out[1].VersionID})
	}
	got := out[0]
	if got.Size != 2048 || got.ETag != in[0].ETag {
		t.Fatalf("scalar fields wrong: %+v", got)
	}
	if got.Erasure.DataBlocks != 4 || got.Erasure.ParityBlocks != 2 {
		t.Fatalf("erasure params wrong: %+v", got.Erasure)
	}
	if got.Metadata["content-type"] != "image/jpeg" || got.Metadata["x-amz-meta-author"] != "tam" {
		t.Fatalf("metadata wrong: %v", got.Metadata)
	}
	if got.Checksum == nil || got.Checksum.Algorithm != "CRC32C" {
		t.Fatalf("checksum wrong: %+v", got.Checksum)
	}
	if !got.ModTime.Equal(mod) {
		t.Fatalf("modtime mismatch: %v vs %v", got.ModTime, mod)
	}
	if !bytes.Equal(got.Parts[0].Checksums[0], bytes.Repeat([]byte{0xab}, 32)) {
		t.Fatalf("part checksum mismatch")
	}
}

func TestMarshalInlineData(t *testing.T) {
	fi := FileInfo{
		VersionID:  "",
		ModTime:    time.Now().UTC(),
		Size:       12,
		InlineData: []byte("shard-bytes!"),
		Erasure:    ErasureInfo{Algorithm: "reedsolomon", DataBlocks: 4, ParityBlocks: 2, Index: 0},
		Parts:      []ObjectPartInfo{{Number: 1, Size: 12, Checksums: [][]byte{bytes.Repeat([]byte{1}, 32)}}},
	}
	raw, err := Marshal([]FileInfo{fi})
	if err != nil {
		t.Fatal(err)
	}
	out, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if !out[0].IsInline() {
		t.Fatal("expected inline")
	}
	if string(out[0].InlineData) != "shard-bytes!" {
		t.Fatalf("inline data wrong: %q", out[0].InlineData)
	}
}

func TestUnmarshalErrors(t *testing.T) {
	if _, err := Unmarshal([]byte{1, 2, 3}); err != ErrTruncated {
		t.Fatalf("short: got %v want ErrTruncated", err)
	}
	bad := append([]byte("XXXX"), make([]byte, 4)...)
	if _, err := Unmarshal(bad); err != ErrBadMagic {
		t.Fatalf("bad magic: got %v", err)
	}
	wrongMajor := []byte("LIO2")
	wrongMajor = append(wrongMajor, 9, 0, 0, 0) // major=9
	if _, err := Unmarshal(wrongMajor); err == nil {
		t.Fatal("expected unsupported major error")
	}
}

func TestEmptyVersions(t *testing.T) {
	raw, err := Marshal(nil)
	if err != nil {
		t.Fatal(err)
	}
	out, err := Unmarshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 0 {
		t.Fatalf("expected 0 versions, got %d", len(out))
	}
}

func TestAddObjectVersionVersioned(t *testing.T) {
	t0 := time.Now()
	var vers []FileInfo
	vers = AddObjectVersion(vers, FileInfo{VersionID: "a", ModTime: t0}, true)
	vers = AddObjectVersion(vers, FileInfo{VersionID: "b", ModTime: t0.Add(time.Second)}, true)
	if len(vers) != 2 {
		t.Fatalf("want 2 versions got %d", len(vers))
	}
	latest, _ := Latest(vers)
	if latest.VersionID != "b" || !latest.IsLatest {
		t.Fatalf("latest wrong: %+v", latest)
	}
	if vers[1].IsLatest {
		t.Fatal("old version still marked latest")
	}
}

func TestAddObjectVersionUnversionedOverwrites(t *testing.T) {
	t0 := time.Now()
	var vers []FileInfo
	vers = AddObjectVersion(vers, FileInfo{Size: 1, ModTime: t0}, false)
	vers = AddObjectVersion(vers, FileInfo{Size: 2, ModTime: t0.Add(time.Second)}, false)
	if len(vers) != 1 {
		t.Fatalf("unversioned should keep one null version, got %d", len(vers))
	}
	if vers[0].Size != 2 {
		t.Fatalf("overwrite failed: size %d", vers[0].Size)
	}
}

func TestDeleteMarkerAndRemove(t *testing.T) {
	t0 := time.Now()
	vers := AddObjectVersion(nil, FileInfo{VersionID: "a", ModTime: t0}, true)
	vers = AddDeleteMarker(vers, "dm1", t0.Add(time.Second))
	latest, _ := Latest(vers)
	if !latest.Deleted {
		t.Fatal("latest should be delete marker")
	}
	vers, removed := RemoveVersion(vers, "dm1")
	if !removed {
		t.Fatal("delete marker not removed")
	}
	latest, _ = Latest(vers)
	if latest.VersionID != "a" || !latest.IsLatest {
		t.Fatalf("after removing marker latest should be a: %+v", latest)
	}
	if _, removed := RemoveVersion(vers, "missing"); removed {
		t.Fatal("removing missing version should report false")
	}
}

func TestQuorumVersionAgreement(t *testing.T) {
	mod := time.Now().UTC()
	// Build per-drive metas where drives carry their own shard index.
	mk := func(idx int) FileInfo {
		fi := sampleVersion("vX", mod)
		fi.Erasure.Index = idx
		return fi
	}
	metas := [][]FileInfo{
		{mk(0)},
		{mk(1)},
		{mk(2)},
		nil, // drive down
	}
	selected, present, ok := QuorumVersion(metas, "", 3)
	if !ok {
		t.Fatal("expected quorum")
	}
	for i := 0; i < 3; i++ {
		if !present[i] {
			t.Fatalf("drive %d should be present", i)
		}
		if selected[i].Erasure.Index != i {
			t.Fatalf("drive %d index = %d", i, selected[i].Erasure.Index)
		}
	}
	if present[3] {
		t.Fatal("downed drive should not be present")
	}
}

// A stale drive that missed the latest write must be reported absent (to be
// healed) while quorum still resolves the latest version.
func TestQuorumVersionStaleDrive(t *testing.T) {
	old := time.Now().Add(-time.Hour).UTC()
	now := time.Now().UTC()
	latest := func(idx int) FileInfo { f := sampleVersion("new", now); f.Erasure.Index = idx; return f }
	stale := func(idx int) FileInfo { f := sampleVersion("old", old); f.Erasure.Index = idx; return f }

	metas := [][]FileInfo{
		{latest(0)},
		{latest(1)},
		{latest(2)},
		{stale(3)}, // missed the latest write
	}
	selected, present, ok := QuorumVersion(metas, "", 3)
	if !ok {
		t.Fatal("expected quorum on latest version")
	}
	if present[3] {
		t.Fatal("stale drive should be reported absent for heal")
	}
	if selected[0].VersionID != "new" {
		t.Fatalf("selected wrong version: %s", selected[0].VersionID)
	}
}

func TestQuorumVersionNoQuorum(t *testing.T) {
	now := time.Now().UTC()
	metas := [][]FileInfo{
		{sampleVersion("a", now)},
		{sampleVersion("b", now)},
		nil,
		nil,
	}
	if _, _, ok := QuorumVersion(metas, "", 3); ok {
		t.Fatal("should not reach quorum when versions disagree and drives are down")
	}
}

func BenchmarkMarshal(b *testing.B) {
	mod := time.Now()
	vers := []FileInfo{sampleVersion("v3", mod), sampleVersion("v2", mod), sampleVersion("v1", mod)}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Marshal(vers); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshal(b *testing.B) {
	mod := time.Now()
	raw, _ := Marshal([]FileInfo{sampleVersion("v3", mod), sampleVersion("v2", mod), sampleVersion("v1", mod)})
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Unmarshal(raw); err != nil {
			b.Fatal(err)
		}
	}
}
