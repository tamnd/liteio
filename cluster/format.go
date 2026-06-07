// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// FormatVersion is the on-disk version of the format.json record. It changes
// only when the record's shape changes, independently of the placement
// AlgorithmVersion it carries.
const FormatVersion = 1

// FormatFile is the filename liteio writes at the root of every drive to record
// that drive's identity within the deployment.
const FormatFile = "format.json"

// Format is a drive's identity record, persisted as format.json at the drive
// root (spec 2020, doc 04.2). A drive that comes up carrying a Format knows
// exactly where it belongs: which deployment, which pool, which erasure set, and
// which position in that set. The record also pins the set size and the
// placement algorithm version so a drive joining a running cluster can be
// checked for agreement before it serves any data.
type Format struct {
	// Version is the format.json schema version (FormatVersion).
	Version int `json:"version"`
	// DeploymentID is the deployment's 16-byte UUID in canonical 8-4-4-4-12 form;
	// it is the placement salt and is identical across every drive in the cluster.
	DeploymentID string `json:"deploymentID"`
	// Pool is the drive's pool index in the cluster's ordered pool list.
	Pool int `json:"pool"`
	// Set is the drive's erasure-set index within its pool.
	Set int `json:"set"`
	// DriveIndex is the drive's position within its set; it fixes which logical
	// shard slot the drive occupies before the per-object permutation is applied.
	DriveIndex int `json:"driveIndex"`
	// SetSize is the number of drives in every set of this pool.
	SetSize int `json:"setSize"`
	// Algorithm is the placement algorithm version (placement.AlgorithmVersion)
	// this drive was formatted under; it is frozen for the life of the pool.
	Algorithm int `json:"algorithm"`
}

// Marshal renders the record as the bytes written to format.json.
func (f Format) Marshal() ([]byte, error) {
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("cluster: marshal format.json: %w", err)
	}
	return b, nil
}

// ParseFormat reads a format.json byte stream back into a Format, rejecting a
// record whose version this build does not understand.
func ParseFormat(b []byte) (Format, error) {
	var f Format
	if err := json.Unmarshal(b, &f); err != nil {
		return Format{}, fmt.Errorf("cluster: parse format.json: %w", err)
	}
	if f.Version != FormatVersion {
		return Format{}, fmt.Errorf("cluster: format.json version %d is not supported (want %d)", f.Version, FormatVersion)
	}
	if err := validateDeploymentID(f.DeploymentID); err != nil {
		return Format{}, err
	}
	return f, nil
}

// Verify checks that a drive's record agrees with the deployment it is joining:
// same deployment ID, same set size, and same placement algorithm. A drive that
// disagrees on any of these must not serve data, since placement would send it
// objects it cannot reconstruct with its peers. Pool, set, and drive index are
// the drive's own coordinates and are not checked here.
func (f Format) Verify(deploymentID string, setSize, algorithm int) error {
	if f.DeploymentID != deploymentID {
		return fmt.Errorf("cluster: drive belongs to deployment %s, not %s", f.DeploymentID, deploymentID)
	}
	if f.SetSize != setSize {
		return fmt.Errorf("cluster: drive set size %d does not match deployment %d", f.SetSize, setSize)
	}
	if f.Algorithm != algorithm {
		return fmt.Errorf("cluster: drive placement algorithm %d does not match deployment %d", f.Algorithm, algorithm)
	}
	return nil
}

// validateDeploymentID checks the canonical 8-4-4-4-12 hex UUID shape.
func validateDeploymentID(s string) error {
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return fmt.Errorf("cluster: deployment ID %q is not a canonical UUID", s)
	}
	for i, r := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			continue
		}
		if !isHexDigit(r) {
			return fmt.Errorf("cluster: deployment ID %q is not a canonical UUID", s)
		}
	}
	return nil
}

// isHexDigit reports whether r is a hexadecimal digit in either case.
func isHexDigit(r rune) bool {
	switch {
	case r >= '0' && r <= '9':
		return true
	case r >= 'a' && r <= 'f':
		return true
	case r >= 'A' && r <= 'F':
		return true
	default:
		return false
	}
}

// formatUUID renders 16 bytes as the canonical 8-4-4-4-12 hex form.
func formatUUID(b [16]byte) string {
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}
