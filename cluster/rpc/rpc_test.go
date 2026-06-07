// SPDX-License-Identifier: Apache-2.0

package rpc

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/vmihailenco/msgpack/v5"
)

type addArgs struct {
	A, B int
}
type addResult struct {
	Sum int
}

// newTestServer mounts a mux with a few representative procedures and returns a
// client pointed at it.
func newTestServer(t *testing.T) *Client {
	t.Helper()
	mux := NewMux()
	mux.EncodeErr = func(err error) string {
		if errors.Is(err, errSentinel) {
			return "sentinel"
		}
		return "x:" + err.Error()
	}

	mux.Register("add", func(_ context.Context, args msgpack.RawMessage, _ io.Reader, resp *Response) error {
		var a addArgs
		if err := msgpack.Unmarshal(args, &a); err != nil {
			return err
		}
		resp.SetResult(addResult{Sum: a.A + a.B})
		return nil
	})
	mux.Register("upload", func(_ context.Context, _ msgpack.RawMessage, body io.Reader, resp *Response) error {
		b, err := io.ReadAll(body)
		if err != nil {
			return err
		}
		resp.SetResult(addResult{Sum: len(b)}) // echo the uploaded byte count
		return nil
	})
	mux.Register("download", func(_ context.Context, args msgpack.RawMessage, _ io.Reader, resp *Response) error {
		var n int
		if err := msgpack.Unmarshal(args, &n); err != nil {
			return err
		}
		resp.SetStream(io.NopCloser(strings.NewReader(strings.Repeat("x", n))), nil)
		return nil
	})
	mux.Register("boom", func(_ context.Context, _ msgpack.RawMessage, _ io.Reader, _ *Response) error {
		return errSentinel
	})
	mux.Register("void", func(_ context.Context, _ msgpack.RawMessage, _ io.Reader, _ *Response) error {
		return nil
	})

	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	c := NewClient(srv.URL, nil)
	c.DecodeErr = func(code string) error {
		if code == "sentinel" {
			return errSentinel
		}
		return errors.New(code)
	}
	return c
}

var errSentinel = errors.New("sentinel error")

func TestUnaryCall(t *testing.T) {
	c := newTestServer(t)
	reply, err := c.Call(context.Background(), "add", addArgs{A: 3, B: 4}, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer reply.Close()
	var res addResult
	if err := reply.Unmarshal(&res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res.Sum != 7 {
		t.Fatalf("sum = %d, want 7", res.Sum)
	}
}

func TestUploadStream(t *testing.T) {
	c := newTestServer(t)
	payload := bytes.Repeat([]byte("a"), 5000)
	reply, err := c.Call(context.Background(), "upload", struct{}{}, bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer reply.Close()
	var res addResult
	if err := reply.Unmarshal(&res); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if res.Sum != 5000 {
		t.Fatalf("server saw %d bytes, want 5000", res.Sum)
	}
}

func TestDownloadStream(t *testing.T) {
	c := newTestServer(t)
	reply, err := c.Call(context.Background(), "download", 1234, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer reply.Close()
	body, err := io.ReadAll(reply.Body)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if len(body) != 1234 {
		t.Fatalf("downloaded %d bytes, want 1234", len(body))
	}
}

func TestErrorRoundTrip(t *testing.T) {
	c := newTestServer(t)
	_, err := c.Call(context.Background(), "boom", struct{}{}, nil)
	if !errors.Is(err, errSentinel) {
		t.Fatalf("err = %v, want errSentinel preserved across the wire", err)
	}
}

func TestVoidCall(t *testing.T) {
	c := newTestServer(t)
	reply, err := c.Call(context.Background(), "void", struct{}{}, nil)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	defer reply.Close()
	// A void result must unmarshal as a no-op, not an error.
	var res addResult
	if err := reply.Unmarshal(&res); err != nil {
		t.Fatalf("Unmarshal void: %v", err)
	}
}

func TestUnknownMethod(t *testing.T) {
	c := newTestServer(t)
	if _, err := c.Call(context.Background(), "nope", struct{}{}, nil); err == nil {
		t.Fatal("expected error for unknown method")
	}
}

func TestOversizeFrameRejected(t *testing.T) {
	// A length prefix beyond the cap must be refused rather than allocated.
	var buf bytes.Buffer
	buf.Write([]byte{0xff, 0xff, 0xff, 0xff}) // ~4 GiB claimed
	if err := readFrame(&buf, &addResult{}); err == nil {
		t.Fatal("expected oversize frame to be rejected")
	}
}

func TestDuplicateRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic on duplicate Register")
		}
	}()
	mux := NewMux()
	mux.Register("m", func(context.Context, msgpack.RawMessage, io.Reader, *Response) error { return nil })
	mux.Register("m", func(context.Context, msgpack.RawMessage, io.Reader, *Response) error { return nil })
}
