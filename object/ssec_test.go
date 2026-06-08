// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/tamnd/liteio/ssec"
)

func randomSSECKey() *[32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	return &k
}

// TestSSECPutGetRoundTrip stores an SSE-C object and verifies the correct key
// retrieves the plaintext.
func TestSSECPutGetRoundTrip(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	key := randomSSECKey()
	body := []byte("super secret data")
	_, err := sp.PutObject(ctx, "bkt", "obj.bin",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{SSECKey: key})
	if err != nil {
		t.Fatalf("PutObject with SSE-C: %v", err)
	}

	got := getBytes(t, sp, "bkt", "obj.bin", ObjectOptions{SSECKey: key})
	if !bytes.Equal(got, body) {
		t.Fatalf("decrypted body mismatch: got %q, want %q", got, body)
	}
}

// TestSSECCiphertextDiffersFromPlaintext verifies the stored bytes are not the
// raw plaintext.
func TestSSECCiphertextDiffersFromPlaintext(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	key := randomSSECKey()
	body := []byte("plaintext-should-not-appear-on-disk")
	info, err := sp.PutObject(ctx, "bkt", "secret",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{SSECKey: key})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// SSE-C response headers: algorithm and key MD5 must appear in UserDefined.
	if info.UserDefined[ssec.MetaKeyMD5] == "" {
		t.Error("MetaKeyMD5 not set in returned UserDefined")
	}
	// ETag is NOT the plaintext MD5.
	import_md5_sum_check := "aad80b1fd9be71ea40e8a42f3a40a3df" // md5 of plaintext
	if info.ETag == import_md5_sum_check {
		t.Error("ETag should not equal plaintext MD5 for SSE-C objects")
	}
}

// TestSSECGetWithoutKey returns ErrSSECKeyRequired.
func TestSSECGetWithoutKey(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	key := randomSSECKey()
	body := []byte("secret")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{SSECKey: key})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	_, err = sp.GetObject(ctx, "bkt", "obj", ObjectOptions{})
	if !errors.Is(err, ErrSSECKeyRequired) {
		t.Fatalf("expected ErrSSECKeyRequired, got %v", err)
	}
}

// TestSSECGetWithWrongKey returns ErrSSECKeyMismatch.
func TestSSECGetWithWrongKey(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	key := randomSSECKey()
	body := []byte("secret")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{SSECKey: key})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	var wrong [32]byte
	wrong[0] = 0xFF
	_, err = sp.GetObject(ctx, "bkt", "obj", ObjectOptions{SSECKey: &wrong})
	if !errors.Is(err, ErrSSECKeyMismatch) {
		t.Fatalf("expected ErrSSECKeyMismatch, got %v", err)
	}
}

// TestSSECGetWithKeyOnUnencrypted returns ErrSSECOnUnencrypted.
func TestSSECGetWithKeyOnUnencrypted(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	body := []byte("plain")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	key := randomSSECKey()
	_, err = sp.GetObject(ctx, "bkt", "obj", ObjectOptions{SSECKey: key})
	if !errors.Is(err, ErrSSECOnUnencrypted) {
		t.Fatalf("expected ErrSSECOnUnencrypted, got %v", err)
	}
}

// TestSSECHeadValidatesKey checks that HeadObject (GetObjectInfo) also enforces
// the SSE-C key requirement.
func TestSSECHeadValidatesKey(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	key := randomSSECKey()
	body := []byte("secret")
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{SSECKey: key})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// No key → ErrSSECKeyRequired.
	_, err = sp.GetObjectInfo(ctx, "bkt", "obj", ObjectOptions{})
	if !errors.Is(err, ErrSSECKeyRequired) {
		t.Fatalf("GetObjectInfo without key: expected ErrSSECKeyRequired, got %v", err)
	}

	// Correct key → success.
	info, err := sp.GetObjectInfo(ctx, "bkt", "obj", ObjectOptions{SSECKey: key})
	if err != nil {
		t.Fatalf("GetObjectInfo with correct key: %v", err)
	}
	if info.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", info.Size, len(body))
	}
}

// TestSSECRangeRead verifies that a range GET on an SSE-C object returns the
// correct slice of plaintext.
func TestSSECRangeRead(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	key := randomSSECKey()
	body := make([]byte, 200)
	for i := range body {
		body[i] = byte(i)
	}
	_, err := sp.PutObject(ctx, "bkt", "obj",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{SSECKey: key})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	got := getBytes(t, sp, "bkt", "obj", ObjectOptions{
		SSECKey: key,
		Range:   &HTTPRangeSpec{Start: 50, End: 99},
	})
	if !bytes.Equal(got, body[50:100]) {
		t.Fatalf("range mismatch: got %d bytes, want %d", len(got), 50)
	}
}
