// SPDX-License-Identifier: Apache-2.0

// Package sses3 implements SSE-S3 (server-managed AES-256-GCM encryption) for
// liteio objects. The server generates a random data encryption key (DEK) per
// object, encrypts the payload with AES-256-GCM, and stores the DEK in a
// KMS-wrapped form inside the object's metadata. The plaintext DEK is never
// persisted.
package sses3

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"github.com/tamnd/liteio/kms"
)

// Metadata keys stored in FileInfo.Metadata for SSE-S3 objects.
const (
	// MetaAlgorithm stores the encryption algorithm identifier.
	MetaAlgorithm = "x-amz-server-side-encryption"
	// MetaWrappedKey stores the base64-encoded KMS-wrapped DEK.
	MetaWrappedKey = "x-amz-server-side-encryption-aws:kms-key-blob"
	// MetaNonce stores the base64-encoded GCM nonce.
	MetaNonce = "x-amz-server-side-encryption-nonce"

	// Algorithm is the S3 algorithm name advertised to clients.
	Algorithm = "AES256"
)

// ErrNotEncrypted is returned when attempting to decrypt an object that has
// no SSE-S3 metadata.
var ErrNotEncrypted = errors.New("sses3: object is not SSE-S3 encrypted")

// ErrDecryptFailed is returned when GCM authentication fails (tampered data or
// wrong DEK).
var ErrDecryptFailed = errors.New("sses3: decryption failed: authentication tag mismatch")

// Encrypt encrypts data with a freshly generated DEK from k. It returns the
// ciphertext (data + GCM tag), the wrapped DEK for metadata storage, and the
// GCM nonce for metadata storage.
func Encrypt(k kms.KMS, data []byte) (ciphertext, wrappedKey, nonce []byte, err error) {
	dek, wrapped, err := k.GenerateDataKey()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("sses3: generate DEK: %w", err)
	}

	block, err := aes.NewCipher(dek[:])
	if err != nil {
		return nil, nil, nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, nil, nil, err
	}
	n := make([]byte, gcm.NonceSize()) // 12 bytes
	if _, err = io.ReadFull(rand.Reader, n); err != nil {
		return nil, nil, nil, err
	}
	// Seal appends tag to dst (starting from n); we want the ciphertext separate.
	ct := gcm.Seal(nil, n, data, nil)
	return ct, wrapped, n, nil
}

// Decrypt recovers the plaintext from ciphertext using the KMS-wrapped DEK and
// GCM nonce stored in object metadata.
func Decrypt(k kms.KMS, ciphertext, wrappedKey, nonce []byte) ([]byte, error) {
	dek, err := k.Decrypt(wrappedKey)
	if err != nil {
		return nil, fmt.Errorf("sses3: unwrap DEK: %w", err)
	}
	block, err := aes.NewCipher(dek[:])
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	plain, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, ErrDecryptFailed
	}
	return plain, nil
}

// ETag computes the opaque ETag for an SSE-S3 object. S3 does not reveal the
// plaintext MD5 for server-encrypted objects; we return hex(MD5(ciphertext))
// to keep the same semantics as the SSE-C implementation.
func ETag(ciphertext []byte) string {
	cs := md5.Sum(ciphertext)
	return hex.EncodeToString(cs[:])
}

// EncodeMetadata encodes the wrapped DEK and GCM nonce as base64 strings
// suitable for storage in FileInfo.Metadata.
func EncodeMetadata(wrappedKey, nonce []byte) (wrappedB64, nonceB64 string) {
	return base64.StdEncoding.EncodeToString(wrappedKey),
		base64.StdEncoding.EncodeToString(nonce)
}

// DecodeMetadata decodes base64-encoded wrapped DEK and nonce from metadata.
func DecodeMetadata(wrappedB64, nonceB64 string) (wrappedKey, nonce []byte, err error) {
	wrappedKey, err = base64.StdEncoding.DecodeString(wrappedB64)
	if err != nil {
		return nil, nil, fmt.Errorf("sses3: decode wrapped key: %w", err)
	}
	nonce, err = base64.StdEncoding.DecodeString(nonceB64)
	if err != nil {
		return nil, nil, fmt.Errorf("sses3: decode nonce: %w", err)
	}
	return wrappedKey, nonce, nil
}
