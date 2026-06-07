// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"testing"

	"github.com/tamnd/liteio/object/placement"
)

func TestSetSizeSelection(t *testing.T) {
	cases := []struct {
		total int
		want  int
	}{
		{4, 4}, {8, 8}, {12, 12}, {15, 15}, {16, 16},
		{30, 15},  // 30 = 15*2
		{32, 16},  // 16*2
		{48, 16},  // 16*3
		{64, 16},  // 16*4
		{24, 12},  // 12*2 (16,15,14,13 do not divide)
		{20, 10},  // 10*2
		{100, 10}, // 10*10
		{9, 9},    // exactly one set
		{10, 10},  // one set
		{200, 10}, // 200 = 10*20
	}
	for _, tc := range cases {
		got, err := SetSize(tc.total)
		if err != nil {
			t.Errorf("SetSize(%d): %v", tc.total, err)
			continue
		}
		if got != tc.want {
			t.Errorf("SetSize(%d) = %d, want %d", tc.total, got, tc.want)
		}
		if tc.total%got != 0 {
			t.Errorf("SetSize(%d) = %d does not divide evenly", tc.total, got)
		}
	}
}

func TestSetSizeErrors(t *testing.T) {
	for _, total := range []int{0, 1, 3, 17, 19, 23} { // primes/too-small above 16
		if _, err := SetSize(total); err == nil {
			t.Errorf("SetSize(%d) should have failed", total)
		}
	}
}

func TestSetSizeWith(t *testing.T) {
	if n, err := SetSizeWith(48, 12); err != nil || n != 12 {
		t.Errorf("SetSizeWith(48,12) = %d,%v; want 12,nil", n, err)
	}
	if _, err := SetSizeWith(48, 7); err == nil {
		t.Error("SetSizeWith with unsupported size 7 should fail")
	}
	if _, err := SetSizeWith(50, 16); err == nil {
		t.Error("SetSizeWith(50,16) should fail; 16 does not divide 50")
	}
}

// hostOf returns the failure-domain key for an endpoint as used by the spread.
func hostOf(e Endpoint) string {
	if e.IsLocal() {
		return "local:" + e.Path
	}
	return e.Host
}

func TestComputeLayoutMaximizesNodeSpread(t *testing.T) {
	// 4 hosts, 8 drives each = 32 drives, set size 16, 2 sets. Each set should
	// draw 4 drives from each of the 4 hosts, so no single host failure can take
	// out more than 4 of a set's 16 shards.
	eps, err := ParsePattern([]string{"https://node{1...4}.lan/mnt/d{1...8}"})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	id, _ := NewDeploymentID()
	layout, err := ComputeLayout(id, eps)
	if err != nil {
		t.Fatalf("ComputeLayout: %v", err)
	}
	if layout.SetSize != 16 {
		t.Fatalf("set size = %d, want 16", layout.SetSize)
	}
	if len(layout.Sets) != 2 {
		t.Fatalf("got %d sets, want 2", len(layout.Sets))
	}
	for s, set := range layout.Sets {
		if len(set) != 16 {
			t.Fatalf("set %d has %d drives, want 16", s, len(set))
		}
		perHost := map[string]int{}
		for _, e := range set {
			perHost[hostOf(e)]++
		}
		if len(perHost) != 4 {
			t.Errorf("set %d spans %d hosts, want 4: %v", s, len(perHost), perHost)
		}
		for h, c := range perHost {
			if c != 4 {
				t.Errorf("set %d has %d drives on %s, want 4 (even spread)", s, c, h)
			}
		}
	}
}

func TestComputeLayoutEveryDrivePlacedOnce(t *testing.T) {
	eps, _ := ParsePattern([]string{"https://n{1...3}/d{1...8}"}) // 24 drives -> size 12, 2 sets
	id, _ := NewDeploymentID()
	layout, err := ComputeLayout(id, eps)
	if err != nil {
		t.Fatalf("ComputeLayout: %v", err)
	}
	seen := map[string]bool{}
	count := 0
	for _, set := range layout.Sets {
		for _, e := range set {
			if seen[e.String()] {
				t.Errorf("drive %s placed in more than one set", e.String())
			}
			seen[e.String()] = true
			count++
		}
	}
	if count != len(eps) {
		t.Errorf("placed %d drives, want %d", count, len(eps))
	}
}

func TestComputeLayoutUnevenHostsStillSpreads(t *testing.T) {
	// 3 hosts with 6,6,4 = 16 drives, set size 16, 1 set. The set must include
	// every host so its tolerance spans all three failure domains.
	var eps []Endpoint
	a, _ := ParsePattern([]string{"http://a/d{1...6}"})
	b, _ := ParsePattern([]string{"http://b/d{1...6}"})
	c, _ := ParsePattern([]string{"http://c/d{1...4}"})
	eps = append(eps, a...)
	eps = append(eps, b...)
	eps = append(eps, c...)
	id, _ := NewDeploymentID()
	layout, err := ComputeLayout(id, eps)
	if err != nil {
		t.Fatalf("ComputeLayout: %v", err)
	}
	if len(layout.Sets) != 1 {
		t.Fatalf("got %d sets, want 1", len(layout.Sets))
	}
	perHost := map[string]int{}
	for _, e := range layout.Sets[0] {
		perHost[hostOf(e)]++
	}
	if len(perHost) != 3 {
		t.Errorf("set spans %d hosts, want 3: %v", len(perHost), perHost)
	}
}

func TestComputeLayoutDeterministic(t *testing.T) {
	eps, _ := ParsePattern([]string{"https://node{1...4}.lan/d{1...4}"})
	id, _ := NewDeploymentID()
	first, _ := ComputeLayout(id, eps)
	for range 20 {
		again, _ := ComputeLayout(id, eps)
		for s := range first.Sets {
			for d := range first.Sets[s] {
				if first.Sets[s][d] != again.Sets[s][d] {
					t.Fatalf("layout not deterministic at set %d drive %d", s, d)
				}
			}
		}
	}
}

func TestComputeLayoutFormats(t *testing.T) {
	eps, _ := ParsePattern([]string{"https://n{1...2}/d{1...8}"}) // 16 drives, size 16, 1 set
	id, _ := NewDeploymentID()
	layout, _ := ComputeLayout(id, eps)
	formats := layout.Formats(3) // pool index 3
	if len(formats) != len(layout.Sets) {
		t.Fatalf("formats has %d sets, want %d", len(formats), len(layout.Sets))
	}
	wantID := formatUUID(id)
	for s := range layout.Sets {
		for d := range layout.Sets[s] {
			f := formats[s][d]
			if f.Version != FormatVersion {
				t.Errorf("format[%d][%d] version = %d", s, d, f.Version)
			}
			if f.DeploymentID != wantID {
				t.Errorf("format[%d][%d] deploymentID = %s, want %s", s, d, f.DeploymentID, wantID)
			}
			if f.Pool != 3 || f.Set != s || f.DriveIndex != d {
				t.Errorf("format[%d][%d] coords = (%d,%d,%d)", s, d, f.Pool, f.Set, f.DriveIndex)
			}
			if f.SetSize != layout.SetSize {
				t.Errorf("format[%d][%d] setSize = %d, want %d", s, d, f.SetSize, layout.SetSize)
			}
			if f.Algorithm != placement.AlgorithmVersion {
				t.Errorf("format[%d][%d] algorithm = %d, want %d", s, d, f.Algorithm, placement.AlgorithmVersion)
			}
		}
	}
}

func BenchmarkComputeLayout(b *testing.B) {
	eps, _ := ParsePattern([]string{"https://node{1...16}.lan/mnt/disk{1...8}"}) // 128 drives
	id, _ := NewDeploymentID()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ComputeLayout(id, eps); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParsePattern(b *testing.B) {
	args := []string{"https://node{1...16}.lan/mnt/disk{1...8}"}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := ParsePattern(args); err != nil {
			b.Fatal(err)
		}
	}
}
