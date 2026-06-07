// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

func TestParseFlagsDefaults(t *testing.T) {
	cfg, err := parseFlags([]string{"--drives", "/d1,/d2,/d3,/d4"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.address != ":9000" {
		t.Errorf("address = %q, want :9000", cfg.address)
	}
	// parity defaults to drives/2.
	if cfg.parity != 2 {
		t.Errorf("parity = %d, want 2", cfg.parity)
	}
}

func TestParseFlagsExplicitParity(t *testing.T) {
	cfg, err := parseFlags([]string{"--drives", "a,b,c", "--parity", "1", "--address", ":7000"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.parity != 1 || cfg.address != ":7000" {
		t.Errorf("got parity=%d address=%q", cfg.parity, cfg.address)
	}
}

func TestSplitNonEmpty(t *testing.T) {
	got := splitNonEmpty(" a , ,b,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("splitNonEmpty = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitNonEmpty[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestDeploymentSaltStable(t *testing.T) {
	a := deploymentSalt("liteio-default")
	b := deploymentSalt("liteio-default")
	if a != b {
		t.Fatal("deploymentSalt is not deterministic")
	}
	if a == deploymentSalt("other") {
		t.Fatal("different IDs should yield different salts")
	}
}

func TestRunRejectsTooFewDrives(t *testing.T) {
	if err := run([]string{"--drives", "/only-one"}); err == nil {
		t.Fatal("expected error for a single drive")
	}
}
