// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"sync"

	"github.com/tamnd/liteio/storage"
)

// SetUsage reports the capacity of one erasure set. Raw figures sum each drive's
// underlying filesystem capacity; usable figures scale the raw down by the
// erasure ratio K/N (K = N - parity), the fraction of raw bytes that holds data
// rather than parity. DrivesReporting is how many of DriveCount drives answered
// the statfs probe: when it is below DriveCount the set's totals are a floor, not
// the full picture, because an offline or unsupported drive contributes nothing.
type SetUsage struct {
	RawTotal        uint64
	RawFree         uint64
	UsableTotal     uint64
	UsableFree      uint64
	DriveCount      int
	DrivesReporting int
}

// DiskUsage is a deployment-wide capacity rollup: the per-set figures plus their
// sums. Unlike StorageInfo it performs a statfs on every drive, so it takes a
// context and is meant for an explicit capacity query rather than a hot poll.
type DiskUsage struct {
	Sets        []SetUsage
	RawTotal    uint64
	RawFree     uint64
	UsableTotal uint64
	UsableFree  uint64
}

// DiskUsage probes every drive's filesystem capacity and aggregates it per set and
// across the deployment. Drives are probed concurrently. A drive that errors (it is
// offline, or its platform does not support the statfs, returning
// storage.ErrDiskInfoUnsupported) is skipped and lowers the set's DrivesReporting;
// the rollup therefore reports the capacity actually visible right now. Drives that
// share one filesystem each report that filesystem's full size, so a deployment
// that packs several drives onto one disk overcounts raw capacity; placement keeps
// drives on distinct filesystems in production, where the sum is exact.
func (sp *ServerPools) DiskUsage(ctx context.Context) DiskUsage {
	du := DiskUsage{}
	for _, p := range sp.pools {
		for _, set := range p.sets {
			su := set.usage(ctx)
			du.Sets = append(du.Sets, su)
			du.RawTotal += su.RawTotal
			du.RawFree += su.RawFree
			du.UsableTotal += su.UsableTotal
			du.UsableFree += su.UsableFree
		}
	}
	return du
}

// usage probes this set's drives concurrently and folds their capacity into a
// SetUsage, scaling the raw totals by K/N to derive usable space.
func (set *erasureSet) usage(ctx context.Context) SetUsage {
	n := len(set.drives)
	infos := make([]storage.DiskInfo, n)
	ok := make([]bool, n)
	var wg sync.WaitGroup
	for i, d := range set.drives {
		wg.Go(func() {
			di, err := d.DiskInfo(ctx)
			if err != nil {
				return
			}
			infos[i] = di
			ok[i] = true
		})
	}
	wg.Wait()

	su := SetUsage{DriveCount: n}
	for i := range n {
		if !ok[i] {
			continue
		}
		su.DrivesReporting++
		su.RawTotal += infos[i].Total
		su.RawFree += infos[i].Free
	}
	// Usable space is the data fraction K/N of raw. Computing it from the raw sum
	// (rather than per drive) keeps the rounding error to a single drive's worth.
	k := uint64(n - set.parity)
	su.UsableTotal = su.RawTotal / uint64(n) * k
	su.UsableFree = su.RawFree / uint64(n) * k
	return su
}
