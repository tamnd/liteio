// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"strings"
	"testing"
)

func sampleFormat() Format {
	id, _ := NewDeploymentID()
	return Format{
		Version:      FormatVersion,
		DeploymentID: formatUUID(id),
		Pool:         1,
		Set:          2,
		DriveIndex:   7,
		SetSize:      16,
		Algorithm:    1,
	}
}

func TestFormatRoundTrip(t *testing.T) {
	f := sampleFormat()
	b, err := f.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	got, err := ParseFormat(b)
	if err != nil {
		t.Fatalf("ParseFormat: %v", err)
	}
	if got != f {
		t.Fatalf("round trip changed record:\n got %+v\nwant %+v", got, f)
	}
}

func TestFormatJSONIsStable(t *testing.T) {
	f := sampleFormat()
	b, _ := f.Marshal()
	s := string(b)
	for _, field := range []string{"version", "deploymentID", "pool", "set", "driveIndex", "setSize", "algorithm"} {
		if !strings.Contains(s, `"`+field+`"`) {
			t.Errorf("format.json missing field %q:\n%s", field, s)
		}
	}
}

func TestParseFormatRejectsUnknownVersion(t *testing.T) {
	f := sampleFormat()
	f.Version = 99
	b, _ := f.Marshal()
	if _, err := ParseFormat(b); err == nil {
		t.Fatal("ParseFormat should reject an unknown version")
	}
}

func TestParseFormatRejectsBadDeploymentID(t *testing.T) {
	f := sampleFormat()
	f.DeploymentID = "not-a-uuid"
	b, _ := f.Marshal()
	if _, err := ParseFormat(b); err == nil {
		t.Fatal("ParseFormat should reject a malformed deployment ID")
	}
}

func TestParseFormatRejectsGarbage(t *testing.T) {
	if _, err := ParseFormat([]byte("{not json")); err == nil {
		t.Fatal("ParseFormat should reject non-JSON")
	}
}

func TestFormatVerify(t *testing.T) {
	f := sampleFormat()
	if err := f.Verify(f.DeploymentID, f.SetSize, f.Algorithm); err != nil {
		t.Fatalf("Verify of a matching record failed: %v", err)
	}

	other, _ := NewDeploymentID()
	if err := f.Verify(formatUUID(other), f.SetSize, f.Algorithm); err == nil {
		t.Error("Verify must reject a different deployment ID")
	}
	if err := f.Verify(f.DeploymentID, f.SetSize+1, f.Algorithm); err == nil {
		t.Error("Verify must reject a different set size")
	}
	if err := f.Verify(f.DeploymentID, f.SetSize, f.Algorithm+1); err == nil {
		t.Error("Verify must reject a different algorithm version")
	}
}

func TestValidateDeploymentID(t *testing.T) {
	id, _ := NewDeploymentID()
	good := formatUUID(id)
	if err := validateDeploymentID(good); err != nil {
		t.Errorf("a real UUID was rejected: %v", err)
	}
	bad := []string{
		"",
		"short",
		strings.Repeat("g", 36),                // right length, non-hex
		"12345678X1234X1234X1234X123456789012", // wrong separators
	}
	for _, s := range bad {
		if err := validateDeploymentID(s); err == nil {
			t.Errorf("validateDeploymentID(%q) should have failed", s)
		}
	}
}
