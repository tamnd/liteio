// SPDX-License-Identifier: Apache-2.0

// Package placement implements liteio's deterministic, lookup-free object
// placement (spec 2020, doc 04).
//
// An object's location is computed from its full namespace key in three stages,
// with no placement database to consult:
//
//   - PoolIndex selects a server pool with a CRC32 over the key, weighted by each
//     pool's free space so a freshly added (emptier) pool fills first.
//   - SetIndex selects an erasure set within the pool with a keyed SipHash-2-4
//     salted by the 16-byte deployment ID, so two clusters place the same key
//     differently and one cluster places it stably.
//   - DriveOrder produces a per-object permutation of the set's drives, rotating
//     which drive holds which shard so the set does not always put parity on the
//     same drives.
//
// Reads recompute the same three functions and go straight to the drives. The
// functions are versioned as a unit (AlgorithmVersion) and recorded in each
// drive's format.json; the rebalancer is the bridge between versions.
package placement

import (
	"encoding/binary"
	"hash/crc32"

	"github.com/dchest/siphash"
)

// AlgorithmVersion identifies the placement function family. It is recorded in
// format.json and frozen for the life of a pool. A future change to placement
// ships as a new version; existing pools keep theirs and the rebalancer moves
// objects between versions when an operator chooses to migrate.
const AlgorithmVersion = 1

// PoolFreeInfo describes a server pool's identity and current free capacity, used
// to weight pool selection toward emptier pools. Free is the available bytes in
// the pool; Index is the pool's position in the cluster's ordered pool list.
type PoolFreeInfo struct {
	Index int
	Free  uint64
}

// PoolIndex selects the server pool for key. Selection is a CRC32 over the key
// mapped into the pools' cumulative free space, so:
//
//   - the same key maps to the same pool for as long as the topology is stable
//     (the weighting comes from free-space ratios, not from the key), and
//   - emptier pools receive proportionally more keys, so new capacity fills first.
//
// It returns the Index field of the chosen pool. With a single pool it returns
// that pool's Index. When every pool reports zero free space it falls back to a
// flat CRC32 modulo so placement still resolves.
func PoolIndex(key string, pools []PoolFreeInfo) int {
	switch len(pools) {
	case 0:
		return 0
	case 1:
		return pools[0].Index
	}

	h := crc32.ChecksumIEEE([]byte(key))

	var totalFree uint64
	for _, p := range pools {
		totalFree += p.Free
	}
	if totalFree == 0 {
		// No free-space signal: distribute flatly and deterministically.
		return pools[int(h)%len(pools)].Index
	}

	// Map h in [0, 2^32) onto a target offset in [0, totalFree) without overflow,
	// then walk the cumulative free space to find the owning pool.
	target := (uint64(h) * totalFree) >> 32
	var cum uint64
	for _, p := range pools {
		cum += p.Free
		if target < cum {
			return p.Index
		}
	}
	// Floating boundary: return the last pool.
	return pools[len(pools)-1].Index
}

// SetIndex selects the erasure set within a pool for key. It is a keyed
// SipHash-2-4 over the key, salted with the 16-byte deployment ID, reduced modulo
// the number of sets. numSets must be >= 1.
func SetIndex(key string, deploymentID [16]byte, numSets int) int {
	if numSets <= 1 {
		return 0
	}
	k0 := binary.LittleEndian.Uint64(deploymentID[0:8])
	k1 := binary.LittleEndian.Uint64(deploymentID[8:16])
	h := siphash.Hash(k0, k1, []byte(key))
	return int(h % uint64(numSets))
}

// DriveOrder returns a per-object permutation of [0, n) deciding which physical
// drive in the set holds logical shard 0, shard 1, and so on. The permutation is
// deterministic for a given key and salt, so reads recompute the same order, and
// is well-distributed across keys so read load and the cost of a single drive
// failure spread evenly across the set.
//
// The salt is the cluster's deployment ID (the same salt used by SetIndex); it
// makes the rotation cluster-unique. n must be >= 1.
func DriveOrder(key string, deploymentID [16]byte, n int) []int {
	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if n <= 1 {
		return order
	}

	// Seed a splitmix64 PRNG from a keyed SipHash of the key so the permutation is
	// deterministic and platform-independent (it does not depend on math/rand
	// internals, which may change across Go versions).
	k0 := binary.LittleEndian.Uint64(deploymentID[0:8])
	k1 := binary.LittleEndian.Uint64(deploymentID[8:16])
	state := siphash.Hash(k0, k1, []byte(key))

	// Fisher-Yates shuffle driven by splitmix64.
	for i := n - 1; i > 0; i-- {
		state += 0x9e3779b97f4a7c15
		z := state
		z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
		z = (z ^ (z >> 27)) * 0x94d049bb133111eb
		z = z ^ (z >> 31)
		j := int(z % uint64(i+1))
		order[i], order[j] = order[j], order[i]
	}
	return order
}
