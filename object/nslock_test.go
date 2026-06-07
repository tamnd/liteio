// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"
)

// TestNSLockExcludesSameKey proves the namespace lock is exclusive per key: while
// one holder has bucket/object, a second acquisition with a short deadline times
// out, and succeeds once the first releases.
func TestNSLockExcludesSameKey(t *testing.T) {
	sp := newLayer(t, 4, 2)
	unlock, err := sp.lockObject(context.Background(), "b", "k")
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}

	short, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := sp.lockObject(short, "b", "k"); !errors.Is(err, ErrOperationTimedOut) {
		t.Fatalf("contended lock = %v, want ErrOperationTimedOut", err)
	}

	unlock()
	unlock2, err := sp.lockObject(context.Background(), "b", "k")
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlock2()
}

// TestNSLockDifferentKeysIndependent proves two different keys never block each
// other.
func TestNSLockDifferentKeysIndependent(t *testing.T) {
	sp := newLayer(t, 4, 2)
	u1, err := sp.lockObject(context.Background(), "b", "k1")
	if err != nil {
		t.Fatalf("lock k1: %v", err)
	}
	defer u1()
	short, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	u2, err := sp.lockObject(short, "b", "k2")
	if err != nil {
		t.Fatalf("lock k2 must not block on k1: %v", err)
	}
	u2()
}

// TestConcurrentPutSameKeyNotTorn stresses the write path: many goroutines PUT
// distinct contents to one key concurrently; the namespace lock plus commit-by-
// rename must leave the key equal to exactly one writer's bytes, never a mix.
func TestConcurrentPutSameKeyNotTorn(t *testing.T) {
	sp := newLayer(t, 6, 3)
	ctx := context.Background()
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}

	const writers = 12
	bodies := make(map[string]bool, writers)
	for i := range writers {
		bodies[fmt.Sprintf("writer-%02d-content", i)] = true
	}

	var wg sync.WaitGroup
	for body := range bodies {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			_, err := sp.PutObject(ctx, "b", "hot", NewPutReader(bytes.NewReader([]byte(body)), int64(len(body))), ObjectOptions{})
			if err != nil {
				t.Errorf("PutObject: %v", err)
			}
		}(body)
	}
	wg.Wait()

	gr, err := sp.GetObject(ctx, "b", "hot", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bodies[string(got)] {
		t.Fatalf("object is a torn write: %q is not any single writer's content", got)
	}
}

// TestConcurrentPutDeleteSameKey runs puts and deletes against one key at once and
// asserts the operations all return cleanly (the lock serializes the put commit
// against the delete) and the final read is either the body or a clean not-found.
func TestConcurrentPutDeleteSameKey(t *testing.T) {
	sp := newLayer(t, 6, 3)
	ctx := context.Background()
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	body := []byte("payload")

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := sp.PutObject(ctx, "b", "k", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{}); err != nil {
				t.Errorf("PutObject: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := sp.DeleteObject(ctx, "b", "k", ObjectOptions{}); err != nil && !errors.Is(err, ErrObjectNotFound) {
				t.Errorf("DeleteObject: %v", err)
			}
		}()
	}
	wg.Wait()

	gr, err := sp.GetObject(ctx, "b", "k", ObjectOptions{})
	switch {
	case err == nil:
		got, _ := io.ReadAll(gr)
		_ = gr.Close()
		if !bytes.Equal(got, body) {
			t.Fatalf("surviving object = %q, want the written payload", got)
		}
	case errors.Is(err, ErrObjectNotFound):
		// A delete won the last race; a clean absence is fine.
	default:
		t.Fatalf("GetObject: %v", err)
	}
}
