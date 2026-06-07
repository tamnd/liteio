// SPDX-License-Identifier: Apache-2.0

// Package buf provides page-aligned buffer pools for the data path (spec 2020,
// doc 06.6).
//
// Two needs are served at once. O_DIRECT file I/O requires the buffer address (and
// the offset and length) to be aligned to the device block size; and the data path
// must not allocate per request, because Go's GC tail latency hurts under load.
// Buffers come from size-classed sync.Pools of page-aligned blocks, are borrowed
// for the duration of a block's encode/write or read/decode, and are returned
// immediately so the live set stays small and GC cycles short.
package buf

import (
	"sync"
	"unsafe"
)

// Alignment is the buffer/offset/length alignment used for O_DIRECT. 4096 bytes
// covers both 512-byte and 4 KiB native block devices.
const Alignment = 4096

// sizeClasses are the pooled buffer sizes, smallest first. A request is served by
// the smallest class that fits; larger requests are allocated directly and not
// pooled (they are rare and pooling them would hold large blocks resident).
var sizeClasses = []int{
	4 << 10,   // 4 KiB: obj.meta and small reads
	64 << 10,  // 64 KiB: small/medium shards
	256 << 10, // 256 KiB
	1 << 20,   // 1 MiB: the default stripe size
	4 << 20,   // 4 MiB
}

var pools = func() []*sync.Pool {
	ps := make([]*sync.Pool, len(sizeClasses))
	for i, sz := range sizeClasses {
		size := sz
		ps[i] = &sync.Pool{New: func() any {
			b := AlignedBlock(size)
			return &b
		}}
	}
	return ps
}()

// AlignedBlock returns a newly allocated slice of length size whose first byte is
// aligned to Alignment. It does not use the pool; callers that want pooling use
// Get/Put.
func AlignedBlock(size int) []byte {
	if size == 0 {
		return make([]byte, 0)
	}
	block := make([]byte, size+Alignment)
	off := alignmentOffset(block)
	return block[off : off+size : off+size]
}

// alignmentOffset returns how many bytes into block the first Alignment-aligned
// address is.
func alignmentOffset(block []byte) int {
	rem := int(uintptr(unsafe.Pointer(&block[0])) & uintptr(Alignment-1))
	if rem == 0 {
		return 0
	}
	return Alignment - rem
}

// Get returns an aligned buffer of exactly length size, drawn from the pool when a
// size class fits. The returned buffer's contents are not zeroed. Return it with
// Put when done.
func Get(size int) []byte {
	for i, classSize := range sizeClasses {
		if size <= classSize {
			bp := pools[i].Get().(*[]byte)
			b := *bp
			return b[:size]
		}
	}
	return AlignedBlock(size)
}

// Put returns a buffer obtained from Get to its pool. Buffers larger than the
// biggest size class (allocated directly by Get) are dropped for the GC. A buffer
// whose capacity does not match a size class (for example a re-sliced foreign
// buffer) is also dropped, so Put is always safe to call.
func Put(b []byte) {
	c := cap(b)
	for i, classSize := range sizeClasses {
		if c == classSize {
			full := b[:classSize]
			pools[i].Put(&full)
			return
		}
	}
}
