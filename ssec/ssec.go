// SPDX-License-Identifier: Apache-2.0

// Package ssec implements S3 SSE-C (server-side encryption with customer-provided
// keys). The S3 wire contract: the client supplies a 32-byte AES-256 key per
// request; the server encrypts/decrypts the object and stores only a base64-encoded
// MD5 of the key for subsequent request validation. The key itself is never stored.
//
// Cipher: AES-256-CTR with a random 12-byte nonce generated at write time and stored
// in object metadata. CTR mode produces a ciphertext equal in length to the plaintext,
// which preserves the object size contract. Range reads are supported by seeking the
// CTR counter to the correct position before decrypting.
package ssec

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/md5" //nolint:gosec // S3 protocol requires MD5 for the key-MD5 header
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
)

// Algorithm is the only value S3 supports for SSE-C.
const Algorithm = "AES256"

// Metadata keys stored in object.FileInfo.Metadata.
const (
	// MetaKeyMD5 holds base64(MD5(customerKey)).
	MetaKeyMD5 = "x-amz-ssec-key-md5"
	// MetaNonce holds base64(12-byte CTR nonce).
	MetaNonce = "x-amz-ssec-nonce"
)

// S3 request/response header names.
const (
	HeaderAlgorithm     = "x-amz-server-side-encryption-customer-algorithm"
	HeaderKey           = "x-amz-server-side-encryption-customer-key"
	HeaderKeyMD5        = "x-amz-server-side-encryption-customer-key-md5"
	CopyHeaderAlgorithm = "x-amz-copy-source-server-side-encryption-customer-algorithm"
	CopyHeaderKey       = "x-amz-copy-source-server-side-encryption-customer-key"
	CopyHeaderKeyMD5    = "x-amz-copy-source-server-side-encryption-customer-key-md5"
)

// Errors returned by the ssec package to the object layer; the S3 handler maps
// them to the appropriate S3 error codes.
var (
	// ErrKeyRequired is returned when a GET/HEAD/DELETE targets an SSE-C object
	// but the request carries no customer key.
	ErrKeyRequired = errors.New("ssec: customer key required for this object")
	// ErrKeyMismatch is returned when the supplied key's MD5 does not match the
	// MD5 stored at write time.
	ErrKeyMismatch = errors.New("ssec: customer key does not match")
)

// ParseKey parses the SSE-C request headers. It returns the 32-byte key and its
// base64-encoded MD5, or ok=false when the headers are absent.
// An error is returned when the headers are present but malformed (wrong algorithm,
// wrong key length, MD5 mismatch).
func ParseKey(r *http.Request) (key [32]byte, keyMD5 string, ok bool, err error) {
	algo := r.Header.Get(HeaderAlgorithm)
	b64Key := r.Header.Get(HeaderKey)
	b64MD5 := r.Header.Get(HeaderKeyMD5)
	if algo == "" && b64Key == "" {
		return key, "", false, nil
	}
	if algo != Algorithm {
		return key, "", false, ErrBadAlgorithm
	}
	raw, decErr := base64.StdEncoding.DecodeString(b64Key)
	if decErr != nil || len(raw) != 32 {
		return key, "", false, ErrBadKey
	}
	copy(key[:], raw)

	if b64MD5 != "" {
		computed := KeyMD5Base64(key)
		if computed != b64MD5 {
			return key, "", false, ErrKeyMD5Mismatch
		}
	}
	return key, KeyMD5Base64(key), true, nil
}

// ParseCopySourceKey parses the SSE-C copy-source request headers (x-amz-copy-source-...).
func ParseCopySourceKey(r *http.Request) (key [32]byte, ok bool, err error) {
	algo := r.Header.Get(CopyHeaderAlgorithm)
	b64Key := r.Header.Get(CopyHeaderKey)
	if algo == "" && b64Key == "" {
		return key, false, nil
	}
	if algo != Algorithm {
		return key, false, ErrBadAlgorithm
	}
	raw, decErr := base64.StdEncoding.DecodeString(b64Key)
	if decErr != nil || len(raw) != 32 {
		return key, false, ErrBadKey
	}
	copy(key[:], raw)
	if b64MD5 := r.Header.Get(CopyHeaderKeyMD5); b64MD5 != "" {
		if KeyMD5Base64(key) != b64MD5 {
			return key, false, ErrKeyMD5Mismatch
		}
	}
	return key, true, nil
}

// KeyMD5Base64 returns the base64-encoded MD5 of a 32-byte key. This is the value
// the server stores in object metadata and echoes back in response headers.
func KeyMD5Base64(key [32]byte) string {
	sum := md5.Sum(key[:]) //nolint:gosec
	return base64.StdEncoding.EncodeToString(sum[:])
}

// ValidateMD5 reports whether the base64-encoded MD5 of key matches stored.
func ValidateMD5(key [32]byte, stored string) bool {
	return KeyMD5Base64(key) == stored
}

// NewNonce returns a cryptographically random 12-byte nonce.
func NewNonce() ([]byte, error) {
	n := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, n); err != nil {
		return nil, err
	}
	return n, nil
}

// Encrypt encrypts src with AES-256-CTR using key and nonce. The output is the
// same length as src. Encrypt is its own inverse (XOR keystream), so the same
// function doubles as Decrypt.
func Encrypt(key [32]byte, nonce []byte, src []byte) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	iv := makeIV(nonce, 0)
	stream := cipher.NewCTR(block, iv)
	dst := make([]byte, len(src))
	stream.XORKeyStream(dst, src)
	return dst, nil
}

// EncryptAt encrypts/decrypts src starting at plaintext offset byteOffset.
// This supports range reads: decrypt the bytes [byteOffset, byteOffset+len(src)).
func EncryptAt(key [32]byte, nonce []byte, src []byte, byteOffset int64) ([]byte, error) {
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	// CTR block number corresponding to byteOffset.
	blockNum := uint64(byteOffset / aes.BlockSize)
	iv := makeIV(nonce, blockNum)
	stream := cipher.NewCTR(block, iv)
	// Skip the leading bytes within the first block that precede byteOffset.
	skip := int(byteOffset % aes.BlockSize)
	if skip > 0 {
		waste := make([]byte, skip)
		stream.XORKeyStream(waste, waste)
	}
	dst := make([]byte, len(src))
	stream.XORKeyStream(dst, src)
	return dst, nil
}

// makeIV builds a 16-byte CTR IV: the first 12 bytes are the nonce, the last 4
// bytes are the big-endian block counter (truncated to 32 bits). This covers
// objects up to 64 GiB (2^32 * 16 bytes), which is the S3 single-PUT limit.
func makeIV(nonce []byte, blockNum uint64) []byte {
	iv := make([]byte, aes.BlockSize)
	copy(iv[:12], nonce)
	counter := uint32(blockNum)
	iv[12] = byte(counter >> 24)
	iv[13] = byte(counter >> 16)
	iv[14] = byte(counter >> 8)
	iv[15] = byte(counter)
	return iv
}

// Sentinel parse errors.
var (
	ErrBadAlgorithm   = errors.New("ssec: algorithm must be AES256")
	ErrBadKey         = errors.New("ssec: key must be a 32-byte base64-encoded value")
	ErrKeyMD5Mismatch = errors.New("ssec: x-amz-server-side-encryption-customer-key-md5 does not match")
)
