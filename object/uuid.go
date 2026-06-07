// SPDX-License-Identifier: Apache-2.0

package object

import (
	"crypto/rand"
	"encoding/hex"
)

// newVersionID returns a random RFC-4122 version-4 UUID string, used as an
// object version identifier. It draws from crypto/rand so version IDs are not
// guessable.
func newVersionID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return formatUUID(b), nil
}

// formatUUID renders 16 bytes as the canonical 8-4-4-4-12 hex form.
func formatUUID(b [16]byte) string {
	var dst [36]byte
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst[:])
}
