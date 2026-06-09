// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/binary"
	"hash/crc32"
	"io"
)

// byteCounter wraps an io.Reader and counts the bytes read through it.
// It is a plain Reader (no Close) so it works with any io.Reader input.
type byteCounter struct {
	r io.Reader
	n int64
}

func (bc *byteCounter) Read(p []byte) (int, error) {
	n, err := bc.r.Read(p)
	bc.n += int64(n)
	return n, err
}

// AWS binary event stream message layout (SelectObjectContent):
//
//   4 bytes  total length       (includes all fields, including both CRC words)
//   4 bytes  headers length
//   4 bytes  prelude CRC        (CRC32/IEEE of the first 8 bytes)
//   N bytes  headers
//   M bytes  payload
//   4 bytes  message CRC        (CRC32/IEEE of everything above)

// encodeHeader encodes a single string-typed event header.
// Format: 1-byte name len, name bytes, 1-byte type (7 = string),
// 2-byte big-endian value len, value bytes.
func encodeHeader(name, value string) []byte {
	nb := []byte(name)
	vb := []byte(value)
	buf := make([]byte, 1+len(nb)+1+2+len(vb))
	off := 0
	buf[off] = byte(len(nb))
	off++
	off += copy(buf[off:], nb)
	buf[off] = 7 // string type
	off++
	binary.BigEndian.PutUint16(buf[off:], uint16(len(vb)))
	off += 2
	copy(buf[off:], vb)
	return buf
}

// writeMessage frames headers + payload into the binary event stream wire format
// and writes it to w.
func writeMessage(w io.Writer, headers []byte, payload []byte) error {
	// total = 4 (total-len) + 4 (headers-len) + 4 (prelude CRC) + len(headers) + len(payload) + 4 (message CRC)
	total := uint32(4 + 4 + 4 + len(headers) + len(payload) + 4)
	hlen := uint32(len(headers))

	prelude := make([]byte, 8)
	binary.BigEndian.PutUint32(prelude[0:], total)
	binary.BigEndian.PutUint32(prelude[4:], hlen)
	preludeCRC := crc32.ChecksumIEEE(prelude)

	preludeCRCBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(preludeCRCBytes, preludeCRC)

	// Build full message (minus the trailing CRC) so we can CRC it in one pass.
	msgWithoutTrail := make([]byte, 0, int(total)-4)
	msgWithoutTrail = append(msgWithoutTrail, prelude...)
	msgWithoutTrail = append(msgWithoutTrail, preludeCRCBytes...)
	msgWithoutTrail = append(msgWithoutTrail, headers...)
	msgWithoutTrail = append(msgWithoutTrail, payload...)

	msgCRC := crc32.ChecksumIEEE(msgWithoutTrail)
	msgCRCBytes := make([]byte, 4)
	binary.BigEndian.PutUint32(msgCRCBytes, msgCRC)

	if _, err := w.Write(msgWithoutTrail); err != nil {
		return err
	}
	_, err := w.Write(msgCRCBytes)
	return err
}

// recordsHeaders builds the headers bytes for a Records event.
func recordsHeaders() []byte {
	var h []byte
	h = append(h, encodeHeader(":message-type", "event")...)
	h = append(h, encodeHeader(":event-type", "Records")...)
	h = append(h, encodeHeader(":content-type", "application/octet-stream")...)
	return h
}

// statsHeaders builds the headers bytes for a Stats event.
func statsHeaders() []byte {
	var h []byte
	h = append(h, encodeHeader(":message-type", "event")...)
	h = append(h, encodeHeader(":event-type", "Stats")...)
	h = append(h, encodeHeader(":content-type", "application/xml")...)
	return h
}

// endHeaders builds the headers bytes for an End event.
func endHeaders() []byte {
	var h []byte
	h = append(h, encodeHeader(":message-type", "event")...)
	h = append(h, encodeHeader(":event-type", "End")...)
	return h
}

// writeRecordsEvent writes a Records event carrying data bytes.
func writeRecordsEvent(w io.Writer, data []byte) error {
	return writeMessage(w, recordsHeaders(), data)
}

// writeStatsEvent writes a Stats event with byte counts.
func writeStatsEvent(w io.Writer, bytesScanned, bytesProcessed, bytesReturned int64) error {
	// Stats payload is XML.
	payload := []byte("<Stats>" +
		"<BytesScanned>" + itoa64(bytesScanned) + "</BytesScanned>" +
		"<BytesProcessed>" + itoa64(bytesProcessed) + "</BytesProcessed>" +
		"<BytesReturned>" + itoa64(bytesReturned) + "</BytesReturned>" +
		"</Stats>")
	return writeMessage(w, statsHeaders(), payload)
}

// writeEndEvent writes the End event that closes the event stream.
func writeEndEvent(w io.Writer) error {
	return writeMessage(w, endHeaders(), nil)
}

// itoa64 converts an int64 to its decimal string representation without
// importing strconv into this file (it is already used in handlers.go; the
// linker deduplicates, so this helper keeps the file self-contained).
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	buf := make([]byte, 20)
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
