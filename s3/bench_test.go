// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/tamnd/liteio/s3/sign"
)

// signedRequest builds a SigV4-signed request against the harness server.
func (h *harness) signedRequest(b *testing.B, method, path string, body []byte) *http.Request {
	b.Helper()
	var rdr io.Reader
	hash := sign.EmptyPayloadHash
	if body != nil {
		rdr = bytes.NewReader(body)
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		b.Fatal(err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	sign.SignHeader(req, testCreds, "us-east-1", hash, time.Now().UTC())
	return req
}

func newBenchHarness(b *testing.B) *harness {
	b.Helper()
	t := &testing.T{}
	return newHarness(t)
}

// send runs a request and drains+closes its body; used for benchmark setup.
func (h *harness) send(b *testing.B, req *http.Request) {
	b.Helper()
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

func BenchmarkS3PutObject(b *testing.B) {
	h := newBenchHarness(b)
	h.send(b, h.signedRequest(b, http.MethodPut, "/bench", nil))
	payload := bytes.Repeat([]byte("x"), 1<<20) // 1 MiB
	client := h.srv.Client()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		resp, err := client.Do(h.signedRequest(b, http.MethodPut, "/bench/obj", payload))
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

func BenchmarkS3GetObject(b *testing.B) {
	h := newBenchHarness(b)
	client := h.srv.Client()
	h.send(b, h.signedRequest(b, http.MethodPut, "/bench", nil))
	payload := bytes.Repeat([]byte("x"), 1<<20)
	h.send(b, h.signedRequest(b, http.MethodPut, "/bench/obj", payload))
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Do(h.signedRequest(b, http.MethodGet, "/bench/obj", nil))
		if err != nil {
			b.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
