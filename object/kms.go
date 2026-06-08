// SPDX-License-Identifier: Apache-2.0

package object

// KMSBackend is the key-management interface the object layer uses for SSE-S3
// and SSE-KMS envelope encryption. Implementations live in the kms package
// (built-in) or in an adapter to an external KMS. A nil KMSBackend disables
// server-managed encryption.
type KMSBackend interface {
	// GenerateDataKey returns a fresh random 256-bit data encryption key as
	// plaintext and a ciphertext suitable for storing in object metadata. The
	// caller must zero the plaintext after use.
	GenerateDataKey() (plaintext [32]byte, ciphertext []byte, err error)

	// Decrypt recovers the plaintext data encryption key from the ciphertext
	// produced by GenerateDataKey.
	Decrypt(ciphertext []byte) (plaintext [32]byte, err error)
}

// WithKMS installs a key-management backend that enables SSE-S3 and SSE-KMS
// encryption. Without this option server-managed encryption is disabled; SSE-C
// still works regardless.
func WithKMS(k KMSBackend) Option {
	return func(sp *ServerPools) {
		if k == nil {
			return
		}
		sp.kms = k
	}
}
