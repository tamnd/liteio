// SPDX-License-Identifier: Apache-2.0

package sses3

import (
	"bytes"
	"testing"

	"github.com/tamnd/liteio/kms"
)

func testKMS() kms.KMS {
	var masterKey [32]byte
	for i := range masterKey {
		masterKey[i] = byte(i + 1)
	}
	return kms.New(masterKey)
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	k := testKMS()
	plaintext := []byte("server-managed AES-256-GCM test data")

	ct, wrappedKey, nonce, err := Encrypt(k, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ct, plaintext) {
		t.Fatal("ciphertext must differ from plaintext")
	}
	// GCM adds a 16-byte authentication tag.
	if len(ct) != len(plaintext)+16 {
		t.Errorf("ciphertext length %d, want %d", len(ct), len(plaintext)+16)
	}

	plain, err := Decrypt(k, ct, wrappedKey, nonce)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(plain, plaintext) {
		t.Fatal("decrypted plaintext does not match original")
	}
}

func TestEncryptNonDeterministic(t *testing.T) {
	k := testKMS()
	data := []byte("same data")
	ct1, w1, n1, _ := Encrypt(k, data)
	ct2, w2, n2, _ := Encrypt(k, data)
	// Two encryptions must produce different ciphertext (different nonce/DEK).
	if bytes.Equal(ct1, ct2) || bytes.Equal(w1, w2) || bytes.Equal(n1, n2) {
		t.Fatal("two Encrypt calls for the same plaintext must produce different output")
	}
}

func TestDecryptTamperedCiphertext(t *testing.T) {
	k := testKMS()
	ct, wrappedKey, nonce, _ := Encrypt(k, []byte("sensitive"))
	ct[len(ct)-1] ^= 0xFF
	_, err := Decrypt(k, ct, wrappedKey, nonce)
	if err == nil {
		t.Fatal("expected error for tampered ciphertext")
	}
}

func TestDecryptTamperedWrappedKey(t *testing.T) {
	k := testKMS()
	ct, wrappedKey, nonce, _ := Encrypt(k, []byte("sensitive"))
	wrappedKey[len(wrappedKey)-1] ^= 0xFF
	_, err := Decrypt(k, ct, wrappedKey, nonce)
	if err == nil {
		t.Fatal("expected error for tampered wrapped key")
	}
}

func TestEncodeDecodeMetadata(t *testing.T) {
	k := testKMS()
	_, wrappedKey, nonce, err := Encrypt(k, []byte("data"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	wB64, nB64 := EncodeMetadata(wrappedKey, nonce)
	if wB64 == "" || nB64 == "" {
		t.Fatal("encoded metadata must not be empty")
	}
	recoveredKey, recoveredNonce, err := DecodeMetadata(wB64, nB64)
	if err != nil {
		t.Fatalf("DecodeMetadata: %v", err)
	}
	if !bytes.Equal(recoveredKey, wrappedKey) {
		t.Fatal("wrapped key mismatch after encode/decode")
	}
	if !bytes.Equal(recoveredNonce, nonce) {
		t.Fatal("nonce mismatch after encode/decode")
	}
}

func TestDecodeMetadataErrors(t *testing.T) {
	_, _, err := DecodeMetadata("not!base64", "AAAA")
	if err == nil {
		t.Fatal("expected error for invalid base64 wrapped key")
	}
	_, _, err = DecodeMetadata("AAAA", "not!base64")
	if err == nil {
		t.Fatal("expected error for invalid base64 nonce")
	}
}

func TestETag(t *testing.T) {
	ct := []byte("ciphertext data")
	tag1 := ETag(ct)
	tag2 := ETag(ct)
	if tag1 != tag2 {
		t.Fatal("ETag must be deterministic")
	}
	if len(tag1) != 32 {
		t.Fatalf("ETag length %d, want 32 (hex MD5)", len(tag1))
	}
	tag3 := ETag([]byte("different data"))
	if tag1 == tag3 {
		t.Fatal("ETag must differ for different inputs")
	}
}
