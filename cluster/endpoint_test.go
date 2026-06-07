// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"strings"
	"testing"
)

func TestParsePatternLocalPaths(t *testing.T) {
	got, err := ParsePattern([]string{"/mnt/disk{1...4}"})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	want := []string{"/mnt/disk1", "/mnt/disk2", "/mnt/disk3", "/mnt/disk4"}
	if len(got) != len(want) {
		t.Fatalf("got %d endpoints, want %d", len(got), len(want))
	}
	for i, e := range got {
		if !e.IsLocal() {
			t.Errorf("endpoint %d should be local: %+v", i, e)
		}
		if e.String() != want[i] {
			t.Errorf("endpoint %d = %q, want %q", i, e.String(), want[i])
		}
	}
}

func TestParsePatternRemoteCartesian(t *testing.T) {
	// Two ranges expand as a cartesian product, leftmost varying slowest.
	got, err := ParsePattern([]string{"https://node{1...2}.lan/mnt/d{1...3}"})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	want := []string{
		"https://node1.lan/mnt/d1", "https://node1.lan/mnt/d2", "https://node1.lan/mnt/d3",
		"https://node2.lan/mnt/d1", "https://node2.lan/mnt/d2", "https://node2.lan/mnt/d3",
	}
	if len(got) != len(want) {
		t.Fatalf("got %d endpoints, want %d: %v", len(got), len(want), got)
	}
	for i, e := range got {
		if e.IsLocal() {
			t.Errorf("endpoint %d should be remote: %+v", i, e)
		}
		if e.Scheme != "https" {
			t.Errorf("endpoint %d scheme = %q, want https", i, e.Scheme)
		}
		if e.String() != want[i] {
			t.Errorf("endpoint %d = %q, want %q", i, e.String(), want[i])
		}
	}
}

func TestParsePatternZeroPadPreserved(t *testing.T) {
	got, err := ParsePattern([]string{"/data/disk{01...12}"})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	if len(got) != 12 {
		t.Fatalf("got %d endpoints, want 12", len(got))
	}
	if got[0].Path != "/data/disk01" {
		t.Errorf("first = %q, want /data/disk01", got[0].Path)
	}
	if got[11].Path != "/data/disk12" {
		t.Errorf("last = %q, want /data/disk12", got[11].Path)
	}
}

func TestParsePatternUnpaddedStaysUnpadded(t *testing.T) {
	got, _ := ParsePattern([]string{"/d{8...10}"})
	want := []string{"/d8", "/d9", "/d10"}
	for i, e := range got {
		if e.Path != want[i] {
			t.Errorf("endpoint %d = %q, want %q", i, e.Path, want[i])
		}
	}
}

func TestParsePatternMultipleArgsConcatenated(t *testing.T) {
	got, err := ParsePattern([]string{"/a{1...2}", "/b{1...2}"})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	want := []string{"/a1", "/a2", "/b1", "/b2"}
	if len(got) != len(want) {
		t.Fatalf("got %d, want %d", len(got), len(want))
	}
	for i, e := range got {
		if e.Path != want[i] {
			t.Errorf("endpoint %d = %q, want %q", i, e.Path, want[i])
		}
	}
}

func TestParsePatternNoRange(t *testing.T) {
	got, err := ParsePattern([]string{"/single/drive"})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/single/drive" {
		t.Fatalf("got %v, want one /single/drive", got)
	}
}

func TestParsePatternErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"empty", nil},
		{"unmatched open", []string{"/d{1...4"}},
		{"unmatched close", []string{"/d1...4}"}},
		{"not a range", []string{"/d{abc}"}},
		{"lo over hi", []string{"/d{9...2}"}},
		{"bad scheme", []string{"ftp://host/d"}},
		{"no host", []string{"http:///d"}},
		{"no path", []string{"https://host"}},
		{"negative", []string{"/d{-1...3}"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParsePattern(tc.args); err == nil {
				t.Fatalf("expected error for %v", tc.args)
			}
		})
	}
}

func TestParsePatternHostWithPort(t *testing.T) {
	got, err := ParsePattern([]string{"http://10.0.0.5:9000/export"})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	if got[0].Host != "10.0.0.5:9000" {
		t.Errorf("host = %q, want 10.0.0.5:9000", got[0].Host)
	}
	if !strings.HasPrefix(got[0].String(), "http://10.0.0.5:9000/") {
		t.Errorf("String = %q", got[0].String())
	}
}
