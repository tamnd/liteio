// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// mustEnableVersioning turns versioning on for the bucket so Object Lock tests
// can proceed; it fails the test immediately if that fails.
func mustEnableVersioning(t *testing.T, sp *ServerPools, bucket string) {
	t.Helper()
	if err := sp.SetBucketVersioning(context.Background(), bucket, VersioningConfig{Enabled: true}); err != nil {
		t.Fatalf("SetBucketVersioning: %v", err)
	}
}

// putBytes is a thin helper that puts a byte slice and returns ObjectInfo.
func putBytes(t *testing.T, sp *ServerPools, bucket, key string, data []byte) ObjectInfo {
	t.Helper()
	info, err := sp.PutObject(context.Background(), bucket, key,
		NewPutReader(strings.NewReader(string(data)), int64(len(data))),
		ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
	return info
}

// TestSetGetObjectLockConfiguration verifies the happy path: set, then get.
func TestSetGetObjectLockConfiguration(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	cfg := ObjectLockConfig{
		Enabled: true,
		Rule: &DefaultRetention{
			Mode: LockModeGov,
			Days: 30,
		},
	}
	if err := sp.SetObjectLockConfiguration(ctx, "bkt", cfg); err != nil {
		t.Fatalf("SetObjectLockConfiguration: %v", err)
	}
	got, err := sp.GetObjectLockConfiguration(ctx, "bkt")
	if err != nil {
		t.Fatalf("GetObjectLockConfiguration: %v", err)
	}
	if !got.Enabled {
		t.Error("expected Enabled=true")
	}
	if got.Rule == nil || got.Rule.Mode != LockModeGov || got.Rule.Days != 30 {
		t.Errorf("unexpected rule: %+v", got.Rule)
	}
}

// TestObjectLockRequiresVersioning verifies that enabling Object Lock on a
// bucket that has not enabled versioning is rejected.
func TestObjectLockRequiresVersioning(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	// Versioning deliberately NOT enabled.
	err := sp.SetObjectLockConfiguration(ctx, "bkt", ObjectLockConfig{Enabled: true})
	if !errors.Is(err, ErrObjectLockRequiresVersioning) {
		t.Fatalf("expected ErrObjectLockRequiresVersioning, got %v", err)
	}
}

// TestGetObjectLockConfigurationMissing verifies the not-found sentinel.
func TestGetObjectLockConfigurationMissing(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	_, err := sp.GetObjectLockConfiguration(ctx, "bkt")
	if !errors.Is(err, ErrNoSuchObjectLockConfiguration) {
		t.Fatalf("expected ErrNoSuchObjectLockConfiguration, got %v", err)
	}
}

// TestSetGetObjectRetention verifies GOVERNANCE retention round-trip.
func TestSetGetObjectRetention(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("data"))
	vid := info.VersionID

	until := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	if err := sp.SetObjectRetention(ctx, "bkt", "obj", vid, LockModeGov, until); err != nil {
		t.Fatalf("SetObjectRetention: %v", err)
	}
	gotMode, gotUntil, err := sp.GetObjectRetention(ctx, "bkt", "obj", vid)
	if err != nil {
		t.Fatalf("GetObjectRetention: %v", err)
	}
	if gotMode != LockModeGov {
		t.Errorf("mode: got %q, want %q", gotMode, LockModeGov)
	}
	if gotUntil != until {
		t.Errorf("retain-until: got %q, want %q", gotUntil, until)
	}
}

// TestGetObjectRetentionMissing verifies ErrNoSuchObjectRetention when the
// version has never had retention set.
func TestGetObjectRetentionMissing(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("data"))
	_, _, err := sp.GetObjectRetention(ctx, "bkt", "obj", info.VersionID)
	if !errors.Is(err, ErrNoSuchObjectRetention) {
		t.Fatalf("expected ErrNoSuchObjectRetention, got %v", err)
	}
}

// TestObjectRetentionBlocksDelete confirms that a locked version cannot be
// permanently deleted.
func TestObjectRetentionBlocksDelete(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("content"))
	vid := info.VersionID

	until := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	if err := sp.SetObjectRetention(ctx, "bkt", "obj", vid, LockModeComp, until); err != nil {
		t.Fatalf("SetObjectRetention: %v", err)
	}

	// Attempt to permanently delete the locked version.
	_, err := sp.DeleteObject(ctx, "bkt", "obj", ObjectOptions{VersionID: vid})
	if !errors.Is(err, ErrObjectLocked) {
		t.Fatalf("expected ErrObjectLocked on delete, got %v", err)
	}

	// Verify the object is still readable.
	body := getBytes(t, sp, "bkt", "obj", ObjectOptions{VersionID: vid})
	if !bytes.Equal(body, []byte("content")) {
		t.Errorf("unexpected body: %q", body)
	}
}

// TestObjectRetentionExpired confirms that a version with a past retain-until
// date can be deleted.
func TestObjectRetentionExpired(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("data"))
	vid := info.VersionID

	// Set a retention date in the past.
	until := time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano)
	if err := sp.SetObjectRetention(ctx, "bkt", "obj", vid, LockModeComp, until); err != nil {
		t.Fatalf("SetObjectRetention: %v", err)
	}

	// Should succeed because retention has expired.
	if _, err := sp.DeleteObject(ctx, "bkt", "obj", ObjectOptions{VersionID: vid}); err != nil {
		t.Fatalf("DeleteObject after expiry: %v", err)
	}
}

// TestComplianceCannotShorten verifies that a COMPLIANCE retain-until date
// cannot be moved to an earlier time.
func TestComplianceCannotShorten(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("data"))
	vid := info.VersionID

	future := time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339Nano)
	if err := sp.SetObjectRetention(ctx, "bkt", "obj", vid, LockModeComp, future); err != nil {
		t.Fatalf("SetObjectRetention (initial): %v", err)
	}

	earlier := time.Now().UTC().Add(1 * time.Hour).Format(time.RFC3339Nano)
	err := sp.SetObjectRetention(ctx, "bkt", "obj", vid, LockModeComp, earlier)
	if !errors.Is(err, ErrComplianceRetentionCannotShorten) {
		t.Fatalf("expected ErrComplianceRetentionCannotShorten, got %v", err)
	}

	// Extending the COMPLIANCE date must succeed.
	longer := time.Now().UTC().Add(72 * time.Hour).Format(time.RFC3339Nano)
	if err := sp.SetObjectRetention(ctx, "bkt", "obj", vid, LockModeComp, longer); err != nil {
		t.Fatalf("SetObjectRetention (extend): %v", err)
	}
}

// TestSetGetObjectLegalHold verifies the legal hold ON/OFF round-trip.
func TestSetGetObjectLegalHold(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("data"))
	vid := info.VersionID

	// Initial state must be OFF.
	status, err := sp.GetObjectLegalHold(ctx, "bkt", "obj", vid)
	if err != nil {
		t.Fatalf("GetObjectLegalHold (initial): %v", err)
	}
	if status != LegalHoldOff {
		t.Errorf("initial status: got %q, want %q", status, LegalHoldOff)
	}

	// Enable legal hold.
	if err := sp.SetObjectLegalHold(ctx, "bkt", "obj", vid, LegalHoldOn); err != nil {
		t.Fatalf("SetObjectLegalHold ON: %v", err)
	}
	status, err = sp.GetObjectLegalHold(ctx, "bkt", "obj", vid)
	if err != nil {
		t.Fatalf("GetObjectLegalHold (after ON): %v", err)
	}
	if status != LegalHoldOn {
		t.Errorf("after ON: got %q, want %q", status, LegalHoldOn)
	}

	// Disable legal hold.
	if err := sp.SetObjectLegalHold(ctx, "bkt", "obj", vid, LegalHoldOff); err != nil {
		t.Fatalf("SetObjectLegalHold OFF: %v", err)
	}
	status, err = sp.GetObjectLegalHold(ctx, "bkt", "obj", vid)
	if err != nil {
		t.Fatalf("GetObjectLegalHold (after OFF): %v", err)
	}
	if status != LegalHoldOff {
		t.Errorf("after OFF: got %q, want %q", status, LegalHoldOff)
	}
}

// TestLegalHoldBlocksDelete verifies that a legal-held version cannot be
// permanently deleted.
func TestLegalHoldBlocksDelete(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("held"))
	vid := info.VersionID

	if err := sp.SetObjectLegalHold(ctx, "bkt", "obj", vid, LegalHoldOn); err != nil {
		t.Fatalf("SetObjectLegalHold ON: %v", err)
	}

	_, err := sp.DeleteObject(ctx, "bkt", "obj", ObjectOptions{VersionID: vid})
	if !errors.Is(err, ErrObjectLocked) {
		t.Fatalf("expected ErrObjectLocked, got %v", err)
	}

	// After lifting hold, delete must succeed.
	if err := sp.SetObjectLegalHold(ctx, "bkt", "obj", vid, LegalHoldOff); err != nil {
		t.Fatalf("SetObjectLegalHold OFF: %v", err)
	}
	if _, err := sp.DeleteObject(ctx, "bkt", "obj", ObjectOptions{VersionID: vid}); err != nil {
		t.Fatalf("DeleteObject after hold lifted: %v", err)
	}
}

// TestCheckObjectLocked exercises the checkObjectLocked helper directly.
func TestCheckObjectLocked(t *testing.T) {
	now := time.Now().UTC()
	future := now.Add(24 * time.Hour).Format(time.RFC3339Nano)
	past := now.Add(-1 * time.Hour).Format(time.RFC3339Nano)

	cases := []struct {
		name    string
		meta    map[string]string
		wantErr bool
	}{
		{"nil meta", nil, false},
		{"empty meta", map[string]string{}, false},
		{"legal hold ON", map[string]string{LegalHoldKey: LegalHoldOn}, true},
		{"legal hold OFF", map[string]string{LegalHoldKey: LegalHoldOff}, false},
		{"GOVERNANCE future", map[string]string{LockModeKey: LockModeGov, LockUntilKey: future}, true},
		{"GOVERNANCE past", map[string]string{LockModeKey: LockModeGov, LockUntilKey: past}, false},
		{"COMPLIANCE future", map[string]string{LockModeKey: LockModeComp, LockUntilKey: future}, true},
		{"COMPLIANCE past", map[string]string{LockModeKey: LockModeComp, LockUntilKey: past}, false},
		{"mode set but no date", map[string]string{LockModeKey: LockModeGov}, false},
		{"bad date ignored", map[string]string{LockModeKey: LockModeComp, LockUntilKey: "not-a-date"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkObjectLocked(tc.meta, now)
			if tc.wantErr && !errors.Is(err, ErrObjectLocked) {
				t.Errorf("expected ErrObjectLocked, got %v", err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

// TestDeleteMarkerNotBlockedByLock confirms that adding a versioned delete
// marker on a locked key is allowed (only permanent version removal is
// blocked).
func TestDeleteMarkerNotBlockedByLock(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	mustEnableVersioning(t, sp, "bkt")

	info := putBytes(t, sp, "bkt", "obj", []byte("content"))

	future := time.Now().UTC().Add(24 * time.Hour).Format(time.RFC3339Nano)
	if err := sp.SetObjectRetention(ctx, "bkt", "obj", info.VersionID, LockModeComp, future); err != nil {
		t.Fatalf("SetObjectRetention: %v", err)
	}

	// Delete WITHOUT a versionId → should write a delete marker, not delete the version.
	if _, err := sp.DeleteObject(ctx, "bkt", "obj", ObjectOptions{}); err != nil {
		t.Fatalf("versioned delete (marker) should not be blocked: %v", err)
	}

	// The original version must still be readable.
	body := getBytes(t, sp, "bkt", "obj", ObjectOptions{VersionID: info.VersionID})
	if !bytes.Equal(body, []byte("content")) {
		t.Errorf("unexpected body after marker: %q", body)
	}
}
