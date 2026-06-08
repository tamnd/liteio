// SPDX-License-Identifier: Apache-2.0

package kms

import (
	"bytes"
	"encoding/base64"
	"testing"
)

func testMasterKey() [32]byte {
	var k [32]byte
	for i := range k {
		k[i] = byte(i + 1)
	}
	return k
}

func TestGenerateDataKeyRoundTrip(t *testing.T) {
	k := New(testMasterKey())
	plain, wrapped, err := k.GenerateDataKey()
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	if len(wrapped) == 0 {
		t.Fatal("wrapped key must not be empty")
	}
	recovered, err := k.Decrypt(wrapped)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if recovered != plain {
		t.Fatal("recovered key does not match original")
	}
}

func TestDecryptWrongMasterKey(t *testing.T) {
	k1 := New(testMasterKey())
	var other [32]byte
	for i := range other {
		other[i] = byte(255 - i)
	}
	k2 := New(other)

	_, wrapped, err := k1.GenerateDataKey()
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	_, err = k2.Decrypt(wrapped)
	if err == nil {
		t.Fatal("expected error when decrypting with wrong master key")
	}
}

func TestWrappedKeysAreUnique(t *testing.T) {
	k := New(testMasterKey())
	_, w1, _ := k.GenerateDataKey()
	_, w2, _ := k.GenerateDataKey()
	if bytes.Equal(w1, w2) {
		t.Fatal("two GenerateDataKey calls must produce different wrapped keys (random nonce)")
	}
}

func TestNewFromBase64(t *testing.T) {
	var raw [32]byte
	for i := range raw {
		raw[i] = byte(i + 1)
	}
	b64 := base64.StdEncoding.EncodeToString(raw[:])
	k, err := NewFromBase64(b64)
	if err != nil {
		t.Fatalf("NewFromBase64: %v", err)
	}
	_, wrapped, err := k.GenerateDataKey()
	if err != nil || len(wrapped) == 0 {
		t.Fatalf("GenerateDataKey after NewFromBase64: %v", err)
	}
}

func TestNewFromBase64Errors(t *testing.T) {
	cases := []struct {
		name string
		b64  string
	}{
		{"empty", ""},
		{"invalid base64", "not-valid-base64!!!"},
		{"16-byte key", "AAAAAAAAAAAAAAAAAAAAAA=="}, // 16 bytes, not 32
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewFromBase64(tc.b64); err == nil {
				t.Fatalf("expected error for %q", tc.b64)
			}
		})
	}
}

func TestDecryptTamperedCiphertext(t *testing.T) {
	k := New(testMasterKey())
	_, wrapped, err := k.GenerateDataKey()
	if err != nil {
		t.Fatalf("GenerateDataKey: %v", err)
	}
	// Flip a byte in the ciphertext body.
	wrapped[len(wrapped)-1] ^= 0xFF
	_, err = k.Decrypt(wrapped)
	if err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}
