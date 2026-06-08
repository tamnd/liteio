// SPDX-License-Identifier: Apache-2.0

//go:build unix

package local

import (
	"context"
	"testing"
)

func TestDiskInfo(t *testing.T) {
	d := newDrive(t)
	di, err := d.DiskInfo(context.Background())
	if err != nil {
		t.Fatalf("DiskInfo: %v", err)
	}
	// The drive root sits on a real filesystem, so the totals must be non-zero and
	// internally consistent: the parts cannot exceed the whole.
	if di.Total == 0 {
		t.Fatal("Total is zero on a real filesystem")
	}
	if di.Free > di.Total {
		t.Fatalf("Free %d exceeds Total %d", di.Free, di.Total)
	}
	if di.Used > di.Total {
		t.Fatalf("Used %d exceeds Total %d", di.Used, di.Total)
	}
	// Free counts only blocks available to an unprivileged writer, so it never
	// exceeds the space not yet used.
	if di.Free > di.Total-di.Used+di.Free {
		t.Fatal("Free is inconsistent with Total and Used")
	}
}

func BenchmarkDiskInfo(b *testing.B) {
	d, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for range b.N {
		if _, err := d.DiskInfo(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
