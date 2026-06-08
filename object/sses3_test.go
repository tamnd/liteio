// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/tamnd/liteio/kms"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// testMasterKey returns a deterministic 32-byte key for tests.
func testMasterKey() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

// newLayerWithKMS creates a ServerPools with a built-in KMS installed.
func newLayerWithKMS(tb testing.TB, n, parity int) *ServerPools {
	tb.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range n {
		d, err := local.New(tb.TempDir())
		if err != nil {
			tb.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	sp, err := NewSingleSet(depID(7), drives, parity, WithKMS(kms.New(testMasterKey())))
	if err != nil {
		tb.Fatalf("NewSingleSet with KMS: %v", err)
	}
	return sp
}

// TestSSES3PutGetRoundTrip stores an SSE-S3 object and verifies the correct
// plaintext is returned on GET.
func TestSSES3PutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	sp := newLayerWithKMS(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	plaintext := []byte("server-managed encryption test data")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(strings.NewReader(string(plaintext)), int64(len(plaintext))),
		ObjectOptions{SSES3: true})
	if err != nil {
		t.Fatalf("PutObject SSE-S3: %v", err)
	}

	body := getBytes(t, sp, "bkt", "obj", ObjectOptions{})
	if !bytes.Equal(body, plaintext) {
		t.Fatalf("decrypted body mismatch: got %q, want %q", body, plaintext)
	}
}

// TestSSES3CiphertextDiffersFromPlaintext verifies the object metadata marks
// the object as SSE-S3 encrypted.
func TestSSES3MetadataPresent(t *testing.T) {
	ctx := context.Background()
	sp := newLayerWithKMS(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	plaintext := []byte("secret data that must not appear on disk")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(strings.NewReader(string(plaintext)), int64(len(plaintext))),
		ObjectOptions{SSES3: true})
	if err != nil {
		t.Fatalf("PutObject SSE-S3: %v", err)
	}

	info, err := sp.GetObjectInfo(ctx, "bkt", "obj", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo: %v", err)
	}
	if info.UserDefined["x-amz-server-side-encryption"] != "AES256" {
		t.Errorf("missing SSE-S3 metadata: %v", info.UserDefined)
	}
}

// TestSSES3NoKMSBlocksGet verifies that a GET of an SSE-S3 object on a layer
// without a KMS returns ErrSSES3NoKMS.
func TestSSES3NoKMSBlocksGet(t *testing.T) {
	ctx := context.Background()
	sp := newLayerWithKMS(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	plaintext := []byte("secret")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(strings.NewReader(string(plaintext)), int64(len(plaintext))),
		ObjectOptions{SSES3: true})
	if err != nil {
		t.Fatalf("PutObject SSE-S3: %v", err)
	}

	// Remove the KMS from the set to simulate a misconfigured node.
	sp.allSets()[0].kms = nil
	_, err = sp.GetObject(ctx, "bkt", "obj", ObjectOptions{})
	if !errors.Is(err, ErrSSES3NoKMS) {
		t.Fatalf("expected ErrSSES3NoKMS, got %v", err)
	}
}

// TestSSES3ETagIsNotPlaintextMD5 confirms the ETag returned for an SSE-S3
// object is not the MD5 of the plaintext.
func TestSSES3ETagIsNotPlaintextMD5(t *testing.T) {
	ctx := context.Background()
	sp := newLayerWithKMS(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	plaintext := []byte("hello liteio SSE-S3")
	info, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(strings.NewReader(string(plaintext)), int64(len(plaintext))),
		ObjectOptions{SSES3: true})
	if err != nil {
		t.Fatalf("PutObject SSE-S3: %v", err)
	}
	sum := md5.Sum(plaintext)
	plaintextMD5 := hex.EncodeToString(sum[:])
	if info.ETag == plaintextMD5 {
		t.Fatal("SSE-S3 ETag must not equal plaintext MD5")
	}
}

// TestSetGetDeleteBucketEncryption verifies the bucket default encryption
// config round-trip.
func TestSetGetDeleteBucketEncryption(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	cfg := BucketEncryptionConfig{Algorithm: "AES256"}
	if err := sp.SetBucketEncryption(ctx, "bkt", cfg); err != nil {
		t.Fatalf("SetBucketEncryption: %v", err)
	}
	got, err := sp.GetBucketEncryption(ctx, "bkt")
	if err != nil {
		t.Fatalf("GetBucketEncryption: %v", err)
	}
	if got.Algorithm != "AES256" {
		t.Errorf("algorithm: got %q, want AES256", got.Algorithm)
	}

	if err := sp.DeleteBucketEncryption(ctx, "bkt"); err != nil {
		t.Fatalf("DeleteBucketEncryption: %v", err)
	}
	_, err = sp.GetBucketEncryption(ctx, "bkt")
	if !errors.Is(err, ErrNoSuchBucketEncryption) {
		t.Fatalf("expected ErrNoSuchBucketEncryption after delete, got %v", err)
	}
}

// TestBucketEncryptionMissingBucket verifies the correct error for an absent bucket.
func TestBucketEncryptionMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	_, err := sp.GetBucketEncryption(ctx, "no-such-bucket")
	if !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("expected ErrBucketNotFound, got %v", err)
	}
}

// TestDeleteBucketEncryptionIdempotent verifies that deleting a non-existent
// encryption config succeeds.
func TestDeleteBucketEncryptionIdempotent(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	if err := sp.DeleteBucketEncryption(ctx, "bkt"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := sp.DeleteBucketEncryption(ctx, "bkt"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

// TestDefaultEncryptionAppliedOnPut verifies that when bucket default
// encryption is set to AES256 and a KMS is configured, PutObject encrypts the
// object even without an explicit SSES3 flag.
func TestDefaultEncryptionAppliedOnPut(t *testing.T) {
	ctx := context.Background()
	sp := newLayerWithKMS(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	if err := sp.SetBucketEncryption(ctx, "bkt", BucketEncryptionConfig{Algorithm: "AES256"}); err != nil {
		t.Fatalf("SetBucketEncryption: %v", err)
	}

	plaintext := []byte("automatically encrypted by default config")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(strings.NewReader(string(plaintext)), int64(len(plaintext))),
		ObjectOptions{}) // no SSES3: true — should be applied automatically
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	body := getBytes(t, sp, "bkt", "obj", ObjectOptions{})
	if !bytes.Equal(body, plaintext) {
		t.Errorf("body mismatch: got %q, want %q", body, plaintext)
	}

	info, err := sp.GetObjectInfo(ctx, "bkt", "obj", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo: %v", err)
	}
	if info.UserDefined["x-amz-server-side-encryption"] != "AES256" {
		t.Errorf("missing SSE-S3 metadata in default-encrypted object: %v", info.UserDefined)
	}
}
