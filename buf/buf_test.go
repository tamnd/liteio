// SPDX-License-Identifier: Apache-2.0

package buf

import (
	"testing"
	"unsafe"
)

func isAligned(b []byte) bool {
	if len(b) == 0 {
		return true
	}
	return uintptr(unsafe.Pointer(&b[0]))&uintptr(Alignment-1) == 0
}

func TestAlignedBlock(t *testing.T) {
	for _, size := range []int{0, 1, 511, 512, 4096, 4097, 1 << 20} {
		b := AlignedBlock(size)
		if len(b) != size {
			t.Fatalf("AlignedBlock(%d) len=%d", size, len(b))
		}
		if !isAligned(b) {
			t.Fatalf("AlignedBlock(%d) not aligned", size)
		}
	}
}

func TestGetReturnsAlignedExactLength(t *testing.T) {
	for _, size := range []int{1, 4096, 4097, 64 << 10, 1 << 20, 4 << 20} {
		b := Get(size)
		if len(b) != size {
			t.Fatalf("Get(%d) len=%d", size, len(b))
		}
		if !isAligned(b) {
			t.Fatalf("Get(%d) not aligned", size)
		}
		Put(b)
	}
}

func TestGetLargerThanMaxClass(t *testing.T) {
	size := 16 << 20 // above the biggest class: allocated directly
	b := Get(size)
	if len(b) != size || !isAligned(b) {
		t.Fatalf("oversized Get failed: len=%d aligned=%v", len(b), isAligned(b))
	}
	Put(b) // must not panic even though it is not pooled
}

func TestPutGetReuse(t *testing.T) {
	b := Get(1 << 20)
	for i := range b {
		b[i] = 0xff
	}
	Put(b)
	// The next Get of the same class should reuse the block (contents not zeroed).
	b2 := Get(1 << 20)
	if cap(b2) != 1<<20 {
		t.Fatalf("reused cap = %d", cap(b2))
	}
	Put(b2)
}

func TestPutForeignBufferSafe(t *testing.T) {
	// A buffer with an off-class capacity must be dropped without panic.
	Put(make([]byte, 100))
	Put(nil)
}

// TestPoolStats checks that PoolStats reflects Get and Put calls.
func TestPoolStats(t *testing.T) {
	getsBefore, putsBefore := PoolStats()

	// Call Get for every pooled size class plus one oversized allocation.
	sizes := append([]int(nil), sizeClasses...)
	sizes = append(sizes, 16<<20) // oversized: not pooled, but still counted
	for _, sz := range sizes {
		b := Get(sz)
		Put(b)
	}
	n := len(sizes)

	getsAfter, putsAfter := PoolStats()
	if got := int(getsAfter - getsBefore); got < n {
		t.Errorf("gets delta = %d, want >= %d", got, n)
	}
	if got := int(putsAfter - putsBefore); got < n {
		t.Errorf("puts delta = %d, want >= %d", got, n)
	}
}

func BenchmarkGetPut1MiB(b *testing.B) {
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		buf := Get(1 << 20)
		Put(buf)
	}
}
