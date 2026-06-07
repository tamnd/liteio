// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"strconv"
	"strings"
)

// chunkSigError and chunkFmtError are the two ways a streaming body fails.
var (
	chunkSigError = &Error{"SignatureDoesNotMatch", "a chunk signature does not match"}
	chunkFmtError = &Error{"IncompleteBody", "the aws-chunked body is malformed or truncated"}
)

// chunkedReader de-frames an aws-chunked request body and, for the signed
// variants, verifies each chunk's signature against the rolling chain seeded by
// the request's header signature (doc 02 §2.3). It yields only the decoded object
// bytes, so the object layer never sees the framing.
type chunkedReader struct {
	br   *bufio.Reader
	vr   *VerifiedRequest
	prev string // previous chunk signature in the chain
	rem  []byte // undelivered bytes of the current chunk
	done bool
	err  error
}

func newChunkedReader(r io.Reader, vr *VerifiedRequest) *chunkedReader {
	return &chunkedReader{br: bufio.NewReader(r), vr: vr, prev: vr.seedSignature}
}

// Read implements io.Reader over the decoded chunk data.
func (c *chunkedReader) Read(p []byte) (int, error) {
	for len(c.rem) == 0 {
		if c.err != nil {
			return 0, c.err
		}
		if c.done {
			return 0, io.EOF
		}
		if err := c.nextChunk(); err != nil {
			c.err = err
			return 0, err
		}
	}
	n := copy(p, c.rem)
	c.rem = c.rem[n:]
	return n, nil
}

// nextChunk reads, verifies, and stages the next framed chunk.
func (c *chunkedReader) nextChunk() error {
	line, err := c.readLine()
	if err != nil {
		return chunkFmtError
	}
	size, sig, perr := parseChunkHeader(line, c.vr.signed)
	if perr != nil {
		return perr
	}

	if size == 0 {
		// The terminating zero-length chunk still carries a signature; verify it,
		// then consume any trailer headers up to the closing blank line.
		if c.vr.signed {
			if verr := c.verifyChunk(nil, sig); verr != nil {
				return verr
			}
		}
		c.done = true
		c.drainTrailers()
		return nil
	}

	data := make([]byte, size)
	if _, err := io.ReadFull(c.br, data); err != nil {
		return chunkFmtError
	}
	if err := c.expectCRLF(); err != nil {
		return err
	}
	if c.vr.signed {
		if verr := c.verifyChunk(data, sig); verr != nil {
			return verr
		}
	}
	c.rem = data
	return nil
}

// verifyChunk checks one chunk's signature and advances the chain on success.
func (c *chunkedReader) verifyChunk(data []byte, sig string) *Error {
	dataHash := sha256.Sum256(data)
	sts := chunkStringToSignAlgo + "\n" +
		c.vr.amzDate + "\n" +
		c.vr.scope.scopeString() + "\n" +
		c.prev + "\n" +
		EmptyPayloadHash + "\n" +
		hex.EncodeToString(dataHash[:])
	want := hex.EncodeToString(hmacSHA256(c.vr.signingKey, sts))
	if subtle.ConstantTimeCompare([]byte(want), []byte(sig)) != 1 {
		return chunkSigError
	}
	c.prev = sig
	return nil
}

// readLine reads one CRLF-terminated line and returns it without the CRLF.
func (c *chunkedReader) readLine() (string, error) {
	line, err := c.br.ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}

// expectCRLF consumes the two-byte CRLF that follows a chunk's data.
func (c *chunkedReader) expectCRLF() *Error {
	var crlf [2]byte
	if _, err := io.ReadFull(c.br, crlf[:]); err != nil || crlf[0] != '\r' || crlf[1] != '\n' {
		return chunkFmtError
	}
	return nil
}

// drainTrailers consumes optional trailer headers after the final chunk, up to
// the closing blank line. Trailer checksums are accepted but not yet validated.
func (c *chunkedReader) drainTrailers() {
	for {
		line, err := c.readLine()
		if err != nil || line == "" {
			return
		}
	}
}

// parseChunkHeader parses "<hex-size>[;chunk-signature=<sig>]". When signed, the
// chunk-signature extension is required.
func parseChunkHeader(line string, signed bool) (int64, string, *Error) {
	head, ext, _ := strings.Cut(line, ";")
	size, err := strconv.ParseInt(strings.TrimSpace(head), 16, 64)
	if err != nil || size < 0 {
		return 0, "", chunkFmtError
	}
	if !signed {
		return size, "", nil
	}
	_, sig, ok := strings.Cut(ext, "chunk-signature=")
	if !ok || sig == "" {
		return 0, "", chunkFmtError
	}
	return size, strings.TrimSpace(sig), nil
}
