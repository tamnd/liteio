// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"testing"
	"time"
)

func TestQuorumVersionsOrdersNewestFirst(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	v1 := sampleVersion("v1", t0)
	v2 := sampleVersion("v2", t0.Add(time.Minute))
	v3 := sampleVersion("v3", t0.Add(2*time.Minute))

	// Every drive agrees on all three versions, recorded in arbitrary order.
	metas := [][]FileInfo{
		{v3, v2, v1},
		{v1, v3, v2},
		{v2, v1, v3},
	}
	got := QuorumVersions(metas, 2)
	if len(got) != 3 {
		t.Fatalf("got %d versions, want 3", len(got))
	}
	if got[0].VersionID != "v3" || got[1].VersionID != "v2" || got[2].VersionID != "v1" {
		t.Fatalf("order = %s,%s,%s, want v3,v2,v1", got[0].VersionID, got[1].VersionID, got[2].VersionID)
	}
	if !got[0].IsLatest || got[1].IsLatest || got[2].IsLatest {
		t.Fatalf("IsLatest flags wrong: %v %v %v", got[0].IsLatest, got[1].IsLatest, got[2].IsLatest)
	}
}

func TestQuorumVersionsDropsSubQuorum(t *testing.T) {
	t0 := time.Unix(1700000000, 0).UTC()
	good := sampleVersion("good", t0)
	lonely := sampleVersion("lonely", t0.Add(time.Minute))

	// "good" is on all three drives; "lonely" only on one, below a quorum of 2.
	metas := [][]FileInfo{
		{lonely, good},
		{good},
		{good},
	}
	got := QuorumVersions(metas, 2)
	if len(got) != 1 || got[0].VersionID != "good" {
		t.Fatalf("got %+v, want only good", got)
	}
}

func TestQuorumVersionsEmpty(t *testing.T) {
	if got := QuorumVersions([][]FileInfo{nil, nil, nil}, 2); len(got) != 0 {
		t.Fatalf("got %d, want 0", len(got))
	}
}
