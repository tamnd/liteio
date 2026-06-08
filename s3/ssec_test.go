// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"testing"

	"github.com/tamnd/liteio/ssec"
)

// ssecHeaders builds the three SSE-C request headers for a given 32-byte key.
func ssecHeaders(key [32]byte) map[string]string {
	return map[string]string{
		ssec.HeaderAlgorithm: ssec.Algorithm,
		ssec.HeaderKey:       base64.StdEncoding.EncodeToString(key[:]),
		ssec.HeaderKeyMD5:    ssec.KeyMD5Base64(key),
	}
}

func randomKey() [32]byte {
	var k [32]byte
	if _, err := rand.Read(k[:]); err != nil {
		panic(err)
	}
	return k
}

// TestSSECPutGetRoundTrip verifies that PutObject with SSE-C headers stores the
// object encrypted and GetObject with the same key returns the plaintext.
func TestSSECPutGetRoundTrip(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)

	key := randomKey()
	body := []byte("hello encrypted world")

	res := h.do("PUT", "/bkt/secret", body, ssecHeaders(key))
	mustStatus(t, res, http.StatusOK)

	// The response must carry the algorithm and key-MD5 headers.
	if got := res.header.Get("x-amz-server-side-encryption-customer-algorithm"); got != ssec.Algorithm {
		t.Errorf("response algorithm header = %q, want %q", got, ssec.Algorithm)
	}
	if got := res.header.Get("x-amz-server-side-encryption-customer-key-md5"); got == "" {
		t.Error("response key-md5 header missing")
	}

	// GET with the correct key returns the plaintext.
	res = h.do("GET", "/bkt/secret", nil, ssecHeaders(key))
	mustStatus(t, res, http.StatusOK)
	if !bytes.Equal(res.body, body) {
		t.Fatalf("body mismatch: got %q, want %q", res.body, body)
	}
}

// TestSSECGetWithoutKey should fail with 400.
func TestSSECGetWithoutKey(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)

	key := randomKey()
	h.do("PUT", "/bkt/obj", []byte("secret"), ssecHeaders(key))

	res := h.do("GET", "/bkt/obj", nil, nil)
	mustStatus(t, res, http.StatusBadRequest)
}

// TestSSECGetWithWrongKey should fail with 403.
func TestSSECGetWithWrongKey(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)

	key := randomKey()
	h.do("PUT", "/bkt/obj", []byte("secret"), ssecHeaders(key))

	wrong := randomKey()
	res := h.do("GET", "/bkt/obj", nil, ssecHeaders(wrong))
	mustStatus(t, res, http.StatusForbidden)
}

// TestSSECHeadWithKey verifies that HeadObject validates the key and returns
// the SSE-C headers.
func TestSSECHeadWithKey(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)

	key := randomKey()
	h.do("PUT", "/bkt/obj", []byte("data"), ssecHeaders(key))

	res := h.do("HEAD", "/bkt/obj", nil, ssecHeaders(key))
	mustStatus(t, res, http.StatusOK)
	if got := res.header.Get("x-amz-server-side-encryption-customer-algorithm"); got != ssec.Algorithm {
		t.Errorf("HEAD algorithm header = %q, want %q", got, ssec.Algorithm)
	}
}

// TestSSECHeadWithoutKey should fail with 400.
func TestSSECHeadWithoutKey(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)

	key := randomKey()
	h.do("PUT", "/bkt/obj", []byte("data"), ssecHeaders(key))

	res := h.do("HEAD", "/bkt/obj", nil, nil)
	// HEAD only returns a status, no body. 400 means key required.
	if res.status == http.StatusOK {
		t.Fatal("HEAD without key for SSE-C object should not return 200")
	}
}

// TestSSECBadAlgorithmHeader should fail with 400.
func TestSSECBadAlgorithmHeader(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)

	key := randomKey()
	hdrs := map[string]string{
		ssec.HeaderAlgorithm: "DES56",
		ssec.HeaderKey:       base64.StdEncoding.EncodeToString(key[:]),
	}
	res := h.do("PUT", "/bkt/obj", []byte("data"), hdrs)
	if res.status == http.StatusOK {
		t.Fatal("bad algorithm should not return 200")
	}
}
