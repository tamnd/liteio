// SPDX-License-Identifier: Apache-2.0

package object

import "encoding/hex"

// DriveInfo is a read-only view of one drive: its stable endpoint identifier and
// whether it is currently reachable. It carries no capacity figures, which the
// StorageAPI does not yet report (a Statfs addition is a separate subsystem).
type DriveInfo struct {
	Endpoint string
	Online   bool
}

// SetInfo is a read-only view of one erasure set: its drives and parity count M.
// The set tolerates losing up to M of its N drives; read quorum is K = N - M.
type SetInfo struct {
	Drives []DriveInfo
	Parity int
}

// PoolInfo is a read-only view of one server pool: its erasure sets.
type PoolInfo struct {
	Sets []SetInfo
}

// StorageInfo is a read-only snapshot of the deployment topology and drive
// reachability. It is the data the admin info and health endpoints (doc 10.2)
// report and the console dashboard renders. It is built by walking the live pools,
// so a drive that has gone offline since boot shows Online=false here.
type StorageInfo struct {
	DeploymentID string
	Pools        []PoolInfo
}

// StorageInfo returns a snapshot of the deployment's pools, sets, and drives with
// each drive's current reachability. It takes no locks beyond reading the drive
// online flags, so it is cheap enough to serve on every dashboard poll.
func (sp *ServerPools) StorageInfo() StorageInfo {
	si := StorageInfo{
		DeploymentID: hex.EncodeToString(sp.deploymentID[:]),
		Pools:        make([]PoolInfo, 0, len(sp.pools)),
	}
	for _, p := range sp.pools {
		pi := PoolInfo{Sets: make([]SetInfo, 0, len(p.sets))}
		for _, set := range p.sets {
			seti := SetInfo{
				Parity: set.parity,
				Drives: make([]DriveInfo, 0, len(set.drives)),
			}
			for _, d := range set.drives {
				seti.Drives = append(seti.Drives, DriveInfo{
					Endpoint: d.String(),
					Online:   d.IsOnline(),
				})
			}
			pi.Sets = append(pi.Sets, seti)
		}
		si.Pools = append(si.Pools, pi)
	}
	return si
}
