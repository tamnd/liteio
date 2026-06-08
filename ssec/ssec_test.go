// SPDX-License-Identifier: Apache-2.0

package ssec

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"testing"
)

func randomKey() [32]byte {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic(err)
	}
	return k
}

func TestKeyMD5Base64RoundTrip(t *testing.T) {
	k := randomKey()
	md5a := KeyMD5Base64(k)
	md5b := KeyMD5Base64(k)
	if md5a != md5b {
		t.Fatal("KeyMD5Base64 not deterministic")
	}
	if !ValidateMD5(k, md5a) {
		t.Fatal("ValidateMD5 should pass for matching key")
	}
	var other [32]byte
	if ValidateMD5(other, md5a) {
		t.Fatal("ValidateMD5 should fail for different key")
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := randomKey()
	nonce, err := NewNonce()
	if err != nil {
		t.Fatalf("NewNonce: %v", err)
	}
	plaintext := []byte("hello liteio SSE-C encryption test data")
	ciphertext, err := Encrypt(key, nonce, plaintext)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("ciphertext should differ from plaintext")
	}
	if len(ciphertext) != len(plaintext) {
		t.Fatalf("ciphertext length %d != plaintext length %d", len(ciphertext), len(plaintext))
	}
	decrypted, err := Encrypt(key, nonce, ciphertext)
	if err != nil {
		t.Fatalf("Decrypt (re-Encrypt): %v", err)
	}
	if !bytes.Equal(decrypted, plaintext) {
		t.Fatal("decrypted != plaintext")
	}
}

func TestEncryptAtRangeRead(t *testing.T) {
	key := randomKey()
	nonce, _ := NewNonce()
	plaintext := make([]byte, 256)
	for i := range plaintext {
		plaintext[i] = byte(i)
	}
	ciphertext, _ := Encrypt(key, nonce, plaintext)

	// Decrypt from offset 50 using EncryptAt.
	partial, err := EncryptAt(key, nonce, ciphertext[50:100], 50)
	if err != nil {
		t.Fatalf("EncryptAt: %v", err)
	}
	if !bytes.Equal(partial, plaintext[50:100]) {
		t.Fatalf("range decrypt mismatch: got %v, want %v", partial[:4], plaintext[50:54])
	}

	// Offset that crosses a 16-byte AES block boundary.
	partial2, err := EncryptAt(key, nonce, ciphertext[13:45], 13)
	if err != nil {
		t.Fatalf("EncryptAt cross-block: %v", err)
	}
	if !bytes.Equal(partial2, plaintext[13:45]) {
		t.Fatal("cross-block range decrypt mismatch")
	}
}

func TestEncryptAtOffset0MatchesEncrypt(t *testing.T) {
	key := randomKey()
	nonce, _ := NewNonce()
	data := []byte("test data for offset zero equivalence")
	full, _ := Encrypt(key, nonce, data)
	partial, _ := EncryptAt(key, nonce, data, 0)
	if !bytes.Equal(full, partial) {
		t.Fatal("EncryptAt(0) != Encrypt")
	}
}

func TestParseKey(t *testing.T) {
	key := randomKey()
	b64Key := base64.StdEncoding.EncodeToString(key[:])
	b64MD5 := KeyMD5Base64(key)

	r, _ := http.NewRequest(http.MethodPut, "/", nil)
	r.Header.Set(HeaderAlgorithm, Algorithm)
	r.Header.Set(HeaderKey, b64Key)
	r.Header.Set(HeaderKeyMD5, b64MD5)

	parsed, md5, ok, err := ParseKey(r)
	if err != nil || !ok {
		t.Fatalf("ParseKey: err=%v ok=%v", err, ok)
	}
	if parsed != key {
		t.Fatal("parsed key mismatch")
	}
	if md5 != b64MD5 {
		t.Fatalf("md5 = %q, want %q", md5, b64MD5)
	}
}

func TestParseKeyAbsent(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "/", nil)
	_, _, ok, err := ParseKey(r)
	if err != nil || ok {
		t.Fatalf("want ok=false err=nil for absent headers, got ok=%v err=%v", ok, err)
	}
}

func TestParseKeyBadMD5(t *testing.T) {
	key := randomKey()
	b64Key := base64.StdEncoding.EncodeToString(key[:])
	r, _ := http.NewRequest(http.MethodPut, "/", nil)
	r.Header.Set(HeaderAlgorithm, Algorithm)
	r.Header.Set(HeaderKey, b64Key)
	r.Header.Set(HeaderKeyMD5, "AAAAAAAAAAAAAAAAAAAAAA==") // wrong MD5
	_, _, _, err := ParseKey(r)
	if err == nil {
		t.Fatal("expected error for wrong key MD5")
	}
}

func TestParseKeyBadAlgorithm(t *testing.T) {
	r, _ := http.NewRequest(http.MethodPut, "/", nil)
	r.Header.Set(HeaderAlgorithm, "DES56")
	r.Header.Set(HeaderKey, "x")
	_, _, _, err := ParseKey(r)
	if err == nil {
		t.Fatal("expected error for unknown algorithm")
	}
}
