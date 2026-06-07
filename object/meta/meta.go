// SPDX-License-Identifier: Apache-2.0

package meta

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/vmihailenco/msgpack/v5"
)

// The obj.meta wire format is a fixed 8-byte header followed by a MessagePack
// body:
//
//	+------------------+
//	| magic  "LIO2"    |  4 bytes
//	| format major     |  2 bytes (LE)
//	| format minor     |  2 bytes (LE)
//	+------------------+
//	| MessagePack body |  variable
//	+------------------+
//
// The major version gates compatibility: a reader rejects a major it does not
// know. Minor bumps are additive; unknown minor fields are ignored on read (and a
// future enhancement preserves them on rewrite).
const (
	Magic       = "LIO2"
	FormatMajor = uint16(1)
	FormatMinor = uint16(0)
	headerSize  = 8
	magicSize   = 4
)

var (
	// ErrBadMagic is returned when the header magic is not "LIO2".
	ErrBadMagic = errors.New("meta: bad obj.meta magic")
	// ErrUnsupportedMajor is returned for a format major the codec does not know.
	ErrUnsupportedMajor = errors.New("meta: unsupported obj.meta format major version")
	// ErrTruncated is returned when the buffer is too short to hold the header.
	ErrTruncated = errors.New("meta: obj.meta truncated")
)

// body is the MessagePack-encoded payload of obj.meta.
type body struct {
	// Versions are ordered newest-first.
	Versions []FileInfo `msgpack:"vers"`
}

// Marshal encodes versions (newest-first) into the obj.meta byte format.
func Marshal(versions []FileInfo) ([]byte, error) {
	payload, err := msgpack.Marshal(body{Versions: versions})
	if err != nil {
		return nil, fmt.Errorf("meta: marshal body: %w", err)
	}
	out := make([]byte, headerSize, headerSize+len(payload))
	copy(out, Magic)
	binary.LittleEndian.PutUint16(out[4:6], FormatMajor)
	binary.LittleEndian.PutUint16(out[6:8], FormatMinor)
	return append(out, payload...), nil
}

// Unmarshal validates the header and decodes the version list.
func Unmarshal(data []byte) ([]FileInfo, error) {
	if len(data) < headerSize {
		return nil, ErrTruncated
	}
	if string(data[:magicSize]) != Magic {
		return nil, ErrBadMagic
	}
	major := binary.LittleEndian.Uint16(data[4:6])
	if major != FormatMajor {
		return nil, fmt.Errorf("%w: %d", ErrUnsupportedMajor, major)
	}
	var b body
	if err := msgpack.Unmarshal(data[headerSize:], &b); err != nil {
		return nil, fmt.Errorf("meta: unmarshal body: %w", err)
	}
	return b.Versions, nil
}
