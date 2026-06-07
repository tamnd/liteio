// SPDX-License-Identifier: Apache-2.0

package placement

import (
	"fmt"
	"math"
	"testing"
)

func depID(b byte) [16]byte {
	var d [16]byte
	for i := range d {
		d[i] = b + byte(i)
	}
	return d
}

func TestPoolIndexDeterministic(t *testing.T) {
	pools := []PoolFreeInfo{{Index: 0, Free: 100}, {Index: 1, Free: 100}, {Index: 2, Free: 100}}
	for _, key := range []string{"bucket/a", "bucket/very/deep/key.ext", "x"} {
		first := PoolIndex(key, pools)
		for i := 0; i < 100; i++ {
			if got := PoolIndex(key, pools); got != first {
				t.Fatalf("PoolIndex(%q) not deterministic: %d vs %d", key, got, first)
			}
		}
	}
}

func TestPoolIndexSingleAndEmpty(t *testing.T) {
	if got := PoolIndex("k", nil); got != 0 {
		t.Fatalf("empty pools: got %d want 0", got)
	}
	if got := PoolIndex("k", []PoolFreeInfo{{Index: 7, Free: 0}}); got != 7 {
		t.Fatalf("single pool: got %d want 7", got)
	}
}

func TestPoolIndexReturnsValidIndex(t *testing.T) {
	pools := []PoolFreeInfo{{Index: 10, Free: 5}, {Index: 20, Free: 5}, {Index: 30, Free: 0}}
	valid := map[int]bool{10: true, 20: true, 30: true}
	for i := 0; i < 1000; i++ {
		got := PoolIndex(fmt.Sprintf("key-%d", i), pools)
		if !valid[got] {
			t.Fatalf("PoolIndex returned invalid index %d", got)
		}
	}
}

// A pool with zero free space must never be selected when others have room.
func TestPoolIndexSkipsFullPool(t *testing.T) {
	pools := []PoolFreeInfo{{Index: 0, Free: 1000}, {Index: 1, Free: 0}}
	for i := 0; i < 5000; i++ {
		if got := PoolIndex(fmt.Sprintf("obj/%d", i), pools); got == 1 {
			t.Fatalf("selected a full pool for key %d", i)
		}
	}
}

// Free-space weighting must bias selection toward the emptier pool roughly in
// proportion to free space.
func TestPoolIndexWeighting(t *testing.T) {
	// Pool 1 has 3x the free space of pool 0; it should get roughly 3x the keys.
	pools := []PoolFreeInfo{{Index: 0, Free: 1000}, {Index: 1, Free: 3000}}
	const n = 200000
	counts := map[int]int{}
	for i := 0; i < n; i++ {
		counts[PoolIndex(fmt.Sprintf("object-key-%d", i), pools)]++
	}
	ratio := float64(counts[1]) / float64(counts[0])
	if ratio < 2.7 || ratio > 3.3 {
		t.Fatalf("weighting off: pool1/pool0 = %.2f, want ~3.0 (counts %v)", ratio, counts)
	}
}

func TestPoolIndexFlatFallbackBalanced(t *testing.T) {
	// All zero free: flat distribution across pools.
	pools := []PoolFreeInfo{{Index: 0, Free: 0}, {Index: 1, Free: 0}, {Index: 2, Free: 0}}
	const n = 90000
	counts := map[int]int{}
	for i := 0; i < n; i++ {
		counts[PoolIndex(fmt.Sprintf("k%d", i), pools)]++
	}
	for idx, c := range counts {
		frac := float64(c) / float64(n)
		if math.Abs(frac-1.0/3.0) > 0.02 {
			t.Fatalf("flat fallback unbalanced for pool %d: %.3f", idx, frac)
		}
	}
}

func TestSetIndexDeterministicAndInRange(t *testing.T) {
	d := depID(1)
	for _, numSets := range []int{1, 2, 4, 7, 16} {
		for i := 0; i < 1000; i++ {
			key := fmt.Sprintf("bucket/key-%d", i)
			got := SetIndex(key, d, numSets)
			if got < 0 || got >= numSets {
				t.Fatalf("SetIndex out of range: %d for numSets %d", got, numSets)
			}
			if again := SetIndex(key, d, numSets); again != got {
				t.Fatalf("SetIndex not deterministic")
			}
		}
	}
}

func TestSetIndexSaltedByDeploymentID(t *testing.T) {
	d1, d2 := depID(1), depID(99)
	differ := 0
	const n = 10000
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("k/%d", i)
		if SetIndex(key, d1, 16) != SetIndex(key, d2, 16) {
			differ++
		}
	}
	// Two different salts should map most keys to different sets.
	if differ < n*8/10 {
		t.Fatalf("deployment salt has little effect: only %d/%d keys differ", differ, n)
	}
}

func TestSetIndexUniformity(t *testing.T) {
	d := depID(5)
	const numSets = 8
	const n = 800000
	counts := make([]int, numSets)
	for i := 0; i < n; i++ {
		counts[SetIndex(fmt.Sprintf("uniform/key/%d", i), d, numSets)]++
	}
	expected := float64(n) / numSets
	for s, c := range counts {
		if dev := math.Abs(float64(c)-expected) / expected; dev > 0.03 {
			t.Fatalf("set %d deviates %.3f from uniform (count %d)", s, dev, c)
		}
	}
}

func TestDriveOrderIsPermutation(t *testing.T) {
	d := depID(2)
	for _, n := range []int{1, 2, 4, 8, 16} {
		for i := 0; i < 500; i++ {
			order := DriveOrder(fmt.Sprintf("obj-%d", i), d, n)
			if len(order) != n {
				t.Fatalf("DriveOrder length %d want %d", len(order), n)
			}
			seen := make([]bool, n)
			for _, v := range order {
				if v < 0 || v >= n {
					t.Fatalf("DriveOrder value %d out of range for n=%d", v, n)
				}
				if seen[v] {
					t.Fatalf("DriveOrder has duplicate %d", v)
				}
				seen[v] = true
			}
		}
	}
}

func TestDriveOrderDeterministic(t *testing.T) {
	d := depID(3)
	key := "bucket/object/path.bin"
	first := DriveOrder(key, d, 12)
	for i := 0; i < 50; i++ {
		got := DriveOrder(key, d, 12)
		for j := range got {
			if got[j] != first[j] {
				t.Fatalf("DriveOrder not deterministic at %d: %v vs %v", j, got, first)
			}
		}
	}
}

// Each logical shard slot should land on every physical drive roughly equally
// across many keys: rotation spreads load and failure cost evenly.
func TestDriveOrderSpreadsShardZero(t *testing.T) {
	d := depID(4)
	const n = 8
	const keys = 400000
	// position[p] counts how often physical drive p holds logical shard 0.
	position := make([]int, n)
	for i := 0; i < keys; i++ {
		order := DriveOrder(fmt.Sprintf("spread/%d", i), d, n)
		position[order[0]]++
	}
	expected := float64(keys) / n
	for p, c := range position {
		if dev := math.Abs(float64(c)-expected) / expected; dev > 0.03 {
			t.Fatalf("shard 0 on drive %d deviates %.3f (count %d)", p, dev, c)
		}
	}
}

func TestDriveOrderTrivialSizes(t *testing.T) {
	d := depID(0)
	if got := DriveOrder("k", d, 0); len(got) != 0 {
		t.Fatalf("n=0 should give empty order, got %v", got)
	}
	if got := DriveOrder("k", d, 1); len(got) != 1 || got[0] != 0 {
		t.Fatalf("n=1 should give [0], got %v", got)
	}
}

func BenchmarkPoolIndex(b *testing.B) {
	pools := []PoolFreeInfo{{Index: 0, Free: 1000}, {Index: 1, Free: 2000}, {Index: 2, Free: 1500}}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = PoolIndex("bucket/some/reasonably/long/object/key.dat", pools)
	}
}

func BenchmarkSetIndex(b *testing.B) {
	d := depID(1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = SetIndex("bucket/some/reasonably/long/object/key.dat", d, 16)
	}
}

func BenchmarkDriveOrder(b *testing.B) {
	d := depID(1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = DriveOrder("bucket/some/reasonably/long/object/key.dat", d, 16)
	}
}
