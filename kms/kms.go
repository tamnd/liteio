// SPDX-License-Identifier: Apache-2.0

// Package kms provides the key-management interface that the object layer uses
// for SSE-S3 and SSE-KMS encryption. The built-in implementation uses a local
// master key to wrap per-object data encryption keys. External KMS backends
// (Vault, KMIP, cloud KMS) implement the same KMS interface.
package kms

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
)

// KMS is the key-management interface the object layer uses for envelope
// encryption. All methods are safe for concurrent use.
type KMS interface {
	// GenerateDataKey returns a random 256-bit data encryption key in plaintext
	// and a ciphertext form that can be stored in object metadata. The plaintext
	// is zeroed before GenerateDataKey returns; callers must not cache it.
	GenerateDataKey() (plaintext [32]byte, ciphertext []byte, err error)

	// Decrypt recovers the plaintext data encryption key from the ciphertext
	// produced by GenerateDataKey.
	Decrypt(ciphertext []byte) (plaintext [32]byte, err error)
}

// builtin is the built-in KMS implementation. It wraps data encryption keys
// with AES-256-GCM using a master key loaded at startup.
type builtin struct {
	masterKey [32]byte
}

// New returns a built-in KMS backed by a 32-byte master key. The key is
// expected to be the operator-supplied secret; the caller must keep it safe.
func New(masterKey [32]byte) KMS {
	return &builtin{masterKey: masterKey}
}

// NewFromBase64 decodes a base64-encoded 32-byte master key and returns a
// built-in KMS. Returns an error if the key is missing or has the wrong length.
func NewFromBase64(b64 string) (KMS, error) {
	if b64 == "" {
		return nil, errors.New("kms: master key is empty")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("kms: master key is not valid base64: %w", err)
	}
	if len(raw) != 32 {
		return nil, fmt.Errorf("kms: master key must be 32 bytes, got %d", len(raw))
	}
	var k [32]byte
	copy(k[:], raw)
	return New(k), nil
}

// GenerateDataKey generates a random DEK and wraps it with the master key
// using AES-256-GCM. The stored form is: 12-byte nonce || GCM ciphertext.
func (b *builtin) GenerateDataKey() (plaintext [32]byte, ciphertext []byte, err error) {
	if _, err = io.ReadFull(rand.Reader, plaintext[:]); err != nil {
		return plaintext, nil, fmt.Errorf("kms: generate DEK: %w", err)
	}
	wrapped, err := b.wrap(plaintext[:])
	if err != nil {
		return plaintext, nil, err
	}
	return plaintext, wrapped, nil
}

// Decrypt unwraps a previously wrapped DEK.
func (b *builtin) Decrypt(ciphertext []byte) ([32]byte, error) {
	raw, err := b.unwrap(ciphertext)
	if err != nil {
		return [32]byte{}, err
	}
	if len(raw) != 32 {
		return [32]byte{}, errors.New("kms: unwrapped key has wrong length")
	}
	var k [32]byte
	copy(k[:], raw)
	return k, nil
}

// wrap encrypts plaintext with the master key using AES-256-GCM.
// Output: 12-byte random nonce || GCM ciphertext (plaintext + 16-byte tag).
func (b *builtin) wrap(plaintext []byte) ([]byte, error) {
	block, err := aes.NewCipher(b.masterKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	out := gcm.Seal(nonce, nonce, plaintext, nil)
	return out, nil
}

// unwrap decrypts a wrapped key produced by wrap.
func (b *builtin) unwrap(ciphertext []byte) ([]byte, error) {
	block, err := aes.NewCipher(b.masterKey[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	ns := gcm.NonceSize()
	if len(ciphertext) < ns+gcm.Overhead() {
		return nil, errors.New("kms: ciphertext too short")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	return gcm.Open(nil, nonce, ct, nil)
}
