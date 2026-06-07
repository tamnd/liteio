// SPDX-License-Identifier: Apache-2.0

// Package rpc is liteio's inter-node transport (spec 2020, docs 06.7, 11.9): a
// framed request/response protocol over HTTP that carries typed messages for the
// distributed StorageAPI and, later, the lock service. It is deliberately ignorant
// of what it carries — callers supply their own argument and result types and
// their own error-code table — so the dependency arrow points up into it
// (storage/remote depends on rpc, never the reverse).
//
// The framing lives in the HTTP body, not in headers: each request body is one
// length-prefixed msgpack "args" frame followed by an optional raw upload stream,
// and each response body is one length-prefixed "envelope" frame (carrying an
// error code and the unary result) followed by an optional raw download stream.
// Framing in the body, rather than packing args into headers, keeps large
// payloads (an object's obj.meta, say) off the header-size limit and lets a single
// uniform shape serve unary, upload, and download calls alike.
package rpc

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/vmihailenco/msgpack/v5"
)

// urlPrefix is the path under which every procedure is mounted.
const urlPrefix = "/liteio-rpc/v1/"

// maxFrameSize caps a single args/envelope frame so a hostile or corrupt length
// prefix cannot force an unbounded allocation. Streamed payloads are not framed
// and are not bounded by this.
const maxFrameSize = 64 << 20

// envelope is the first frame of every response: an error code (empty on success)
// and the msgpack-encoded result for a unary call.
type envelope struct {
	Err    string             `msgpack:"e,omitempty"`
	Result msgpack.RawMessage `msgpack:"r,omitempty"`
}

// writeFrame writes v as a 4-byte big-endian length prefix followed by its
// msgpack encoding.
func writeFrame(w io.Writer, v any) error {
	b, err := msgpack.Marshal(v)
	if err != nil {
		return err
	}
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(len(b)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// readFrame reads one length-prefixed msgpack frame into v.
func readFrame(r io.Reader, v any) error {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n > maxFrameSize {
		return fmt.Errorf("rpc: frame of %d bytes exceeds %d-byte limit", n, maxFrameSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return msgpack.Unmarshal(buf, v)
}

// --- server ---------------------------------------------------------------

// Response is how a Handler returns to the caller. SetResult records a unary
// result to marshal into the envelope; SetStream records a payload to copy after
// the envelope (and a Closer to release when the copy is done). A handler uses at
// most one of them.
type Response struct {
	result any
	stream io.Reader
	closer io.Closer
}

// SetResult records the unary result value (marshaled into the response envelope).
func (r *Response) SetResult(v any) { r.result = v }

// SetStream records a payload streamed to the caller after the envelope. closer,
// if non-nil, is closed once the payload has been copied.
func (r *Response) SetStream(rd io.Reader, closer io.Closer) { r.stream, r.closer = rd, closer }

// Handler runs one procedure: it decodes its arguments from args, reads any
// streamed request payload from body, and reports its outcome through resp.
// Returning a non-nil error sends that error's code to the caller in place of a
// result.
type Handler func(ctx context.Context, args msgpack.RawMessage, body io.Reader, resp *Response) error

// Mux is an http.Handler that dispatches framed RPC requests to registered
// procedures. EncodeErr, if set, maps a handler error to the wire code the client
// will decode; the default uses the error's message.
type Mux struct {
	mu        sync.RWMutex
	methods   map[string]Handler
	EncodeErr func(error) string
}

// NewMux returns an empty Mux.
func NewMux() *Mux { return &Mux{methods: map[string]Handler{}} }

// Register mounts h under method. It panics on a duplicate registration, since
// that is a programming error at wiring time.
func (m *Mux) Register(method string, h Handler) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, dup := m.methods[method]; dup {
		panic("rpc: duplicate method " + method)
	}
	m.methods[method] = h
}

// ServeHTTP implements http.Handler. Transport-level failures use HTTP status
// codes; logical (handler) errors are always carried in the 200 response envelope
// so the client can map them back to typed errors.
func (m *Mux) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, urlPrefix) {
		http.NotFound(w, r)
		return
	}
	method := strings.TrimPrefix(r.URL.Path, urlPrefix)
	m.mu.RLock()
	h, ok := m.methods[method]
	m.mu.RUnlock()
	if !ok {
		http.Error(w, "rpc: unknown method "+method, http.StatusNotFound)
		return
	}

	var args msgpack.RawMessage
	if err := readFrame(r.Body, &args); err != nil {
		http.Error(w, "rpc: bad args frame: "+err.Error(), http.StatusBadRequest)
		return
	}

	resp := &Response{}
	herr := h(r.Context(), args, r.Body, resp)

	w.Header().Set("Content-Type", "application/octet-stream")
	env := envelope{}
	switch {
	case herr != nil:
		env.Err = m.encodeErr(herr)
	case resp.result != nil:
		b, err := msgpack.Marshal(resp.result)
		if err != nil {
			env.Err = m.encodeErr(fmt.Errorf("rpc: marshal result: %w", err))
		} else {
			env.Result = b
		}
	}
	if err := writeFrame(w, env); err != nil {
		return // connection is already broken; nothing more to say
	}
	if env.Err == "" && resp.stream != nil {
		_, _ = io.Copy(w, resp.stream)
	}
	if resp.closer != nil {
		_ = resp.closer.Close()
	}
}

func (m *Mux) encodeErr(err error) string {
	if m.EncodeErr != nil {
		return m.EncodeErr(err)
	}
	return err.Error()
}

// --- client ----------------------------------------------------------------

// Client calls procedures on one peer's Mux over a pooled HTTP connection.
// DecodeErr, if set, maps a wire error code back to a typed error.
type Client struct {
	base      string
	hc        *http.Client
	DecodeErr func(string) error
}

// NewClient returns a Client targeting baseURL (the peer's scheme://host[:port]).
// A nil hc uses a default client, whose transport pools and reuses connections.
func NewClient(baseURL string, hc *http.Client) *Client {
	if hc == nil {
		hc = &http.Client{}
	}
	return &Client{base: strings.TrimRight(baseURL, "/"), hc: hc}
}

// Reply is the result of a Call: the unary Result (decode with Unmarshal) and the
// response Body positioned just after the envelope, which carries the streamed
// download payload for streaming methods. The caller must Close it.
type Reply struct {
	Result msgpack.RawMessage
	Body   io.ReadCloser
}

// Unmarshal decodes the unary result into v. An empty result (a void procedure)
// is a no-op.
func (r *Reply) Unmarshal(v any) error {
	if len(r.Result) == 0 {
		return nil
	}
	return msgpack.Unmarshal(r.Result, v)
}

// Close releases the underlying response body.
func (r *Reply) Close() error { return r.Body.Close() }

// Call invokes method with args and an optional upload stream, returning the
// reply. On a logical error from the handler it returns the decoded error and a
// nil reply (the body is already drained and closed).
func (c *Client) Call(ctx context.Context, method string, args any, upload io.Reader) (*Reply, error) {
	frame := &bytes.Buffer{}
	if err := writeFrame(frame, args); err != nil {
		return nil, err
	}
	var body io.Reader = frame
	if upload != nil {
		body = io.MultiReader(frame, upload)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+urlPrefix+method, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, fmt.Errorf("rpc: %s: %s", method, resp.Status)
	}
	var env envelope
	if err := readFrame(resp.Body, &env); err != nil {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("rpc: %s: read envelope: %w", method, err)
	}
	if env.Err != "" {
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return nil, c.decodeErr(env.Err)
	}
	return &Reply{Result: env.Result, Body: resp.Body}, nil
}

func (c *Client) decodeErr(code string) error {
	if c.DecodeErr != nil {
		return c.DecodeErr(code)
	}
	return fmt.Errorf("rpc: remote error: %s", code)
}
