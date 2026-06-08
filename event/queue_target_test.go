// SPDX-License-Identifier: Apache-2.0

package event

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"sync"
	"testing"
	"time"
)

// TestFileQueueTargetWrites delivers two envelopes, closes the target, then
// reads the file back and checks that it contains exactly two JSON lines.
func TestFileQueueTargetWrites(t *testing.T) {
	f, err := os.CreateTemp("", "liteio-queue-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	qt, err := NewFileQueueTarget(path)
	if err != nil {
		t.Fatalf("NewFileQueueTarget: %v", err)
	}

	env1 := Envelope{Records: []Record{{EventName: ObjectCreatedPut, S3: S3Entity{Bucket: BucketID{Name: "b1"}, Object: ObjectID{Key: "k1"}}}}}
	env2 := Envelope{Records: []Record{{EventName: ObjectRemovedDelete, S3: S3Entity{Bucket: BucketID{Name: "b2"}, Object: ObjectID{Key: "k2"}}}}}

	if err := qt.Deliver(context.Background(), env1); err != nil {
		t.Fatalf("Deliver 1: %v", err)
	}
	if err := qt.Deliver(context.Background(), env2); err != nil {
		t.Fatalf("Deliver 2: %v", err)
	}
	if err := qt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	var lines []Envelope
	sc := bufio.NewScanner(rf)
	for sc.Scan() {
		var env Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatalf("unmarshal line: %v", err)
		}
		lines = append(lines, env)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2", len(lines))
	}
	if lines[0].Records[0].S3.Bucket.Name != "b1" {
		t.Errorf("line 0 bucket = %q, want b1", lines[0].Records[0].S3.Bucket.Name)
	}
	if lines[1].Records[0].S3.Bucket.Name != "b2" {
		t.Errorf("line 1 bucket = %q, want b2", lines[1].Records[0].S3.Bucket.Name)
	}
}

// TestFileQueueTargetConcurrent delivers many envelopes concurrently and verifies
// no data corruption.
func TestFileQueueTargetConcurrent(t *testing.T) {
	f, err := os.CreateTemp("", "liteio-queue-concurrent-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	t.Cleanup(func() { os.Remove(path) })

	qt, err := NewFileQueueTarget(path)
	if err != nil {
		t.Fatalf("NewFileQueueTarget: %v", err)
	}

	const n = 50
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := Envelope{Records: []Record{{EventName: ObjectCreatedPut, EventTime: time.Now()}}}
			_ = i
			if err := qt.Deliver(context.Background(), env); err != nil {
				t.Errorf("Deliver: %v", err)
			}
		}(i)
	}
	wg.Wait()
	if err := qt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	rf, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer rf.Close()

	count := 0
	sc := bufio.NewScanner(rf)
	for sc.Scan() {
		var env Envelope
		if err := json.Unmarshal(sc.Bytes(), &env); err != nil {
			t.Fatalf("corrupt JSON on line %d: %v", count+1, err)
		}
		count++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if count != n {
		t.Errorf("got %d lines, want %d", count, n)
	}
}
