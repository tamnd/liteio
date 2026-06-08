// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"net/http"

	"github.com/tamnd/liteio/object"
)

// InfoSource is the slice of the object layer the info and health endpoints read.
// StorageInfo is a cheap topology-and-reachability snapshot the health poll uses;
// DiskUsage performs a statfs on every drive, so it carries a context and the info
// endpoint calls it once per request. *object.ServerPools satisfies both. The
// interface keeps admin's dependency on the object layer to two read-only calls and
// lets the endpoints be tested against a fake.
type InfoSource interface {
	StorageInfo() object.StorageInfo
	DiskUsage(ctx context.Context) object.DiskUsage
}

// driveInfo is the wire shape of one drive in the info response.
type driveInfo struct {
	Endpoint string `json:"endpoint"`
	Online   bool   `json:"online"`
}

// setInfo is the wire shape of one erasure set: its drives, parity, and the
// derived counts an operator reads at a glance. ReadQuorum is K = N - M, the
// number of drives that must be online to read; a set with fewer is unavailable.
type setInfo struct {
	Drives      []driveInfo `json:"drives"`
	Parity      int         `json:"parity"`
	DriveCount  int         `json:"driveCount"`
	OnlineCount int         `json:"onlineCount"`
	ReadQuorum  int         `json:"readQuorum"`
	Healthy     bool        `json:"healthy"`   // every drive online
	Available   bool        `json:"available"` // at least read quorum online

	// Capacity, in bytes, of the filesystems backing the set. Raw is the underlying
	// total; usable is the data fraction (raw scaled by readQuorum/driveCount).
	// DrivesReporting is how many drives answered the statfs probe; below DriveCount
	// the figures are a floor, since an offline or unsupported drive adds nothing.
	RawCapacity     uint64 `json:"rawCapacity"`
	RawFree         uint64 `json:"rawFree"`
	UsableCapacity  uint64 `json:"usableCapacity"`
	UsableFree      uint64 `json:"usableFree"`
	DrivesReporting int    `json:"drivesReporting"`
}

// poolInfo is the wire shape of one server pool.
type poolInfo struct {
	Sets []setInfo `json:"sets"`
}

// serverInfoResponse is the full info payload: version, deployment, topology, and
// the aggregate drive counts across every set.
type serverInfoResponse struct {
	Version      string     `json:"version"`
	DeploymentID string     `json:"deploymentId"`
	Pools        []poolInfo `json:"pools"`
	PoolCount    int        `json:"poolCount"`
	SetCount     int        `json:"setCount"`
	DriveCount   int        `json:"driveCount"`
	OnlineDrives int        `json:"onlineDriveCount"`

	// Deployment-wide capacity in bytes, summed over every set.
	RawCapacity    uint64 `json:"rawCapacity"`
	RawFree        uint64 `json:"rawFree"`
	UsableCapacity uint64 `json:"usableCapacity"`
	UsableFree     uint64 `json:"usableFree"`
}

// healthResponse is the lighter health summary: an overall status plus the count of
// sets that are degraded (a drive down) or unavailable (below read quorum).
type healthResponse struct {
	Status      string `json:"status"` // "healthy", "degraded", or "unavailable"
	Sets        int    `json:"setCount"`
	Degraded    int    `json:"degradedSetCount"`
	Unavailable int    `json:"unavailableSetCount"`
}

// serverInfo reports the full deployment topology, drive reachability, and capacity
// (doc 10.2). It pairs the topology snapshot with a statfs-backed usage probe; both
// walk pools then sets in the same order, so the usage of the Nth set lines up by
// index.
func (s *Server) serverInfo(w http.ResponseWriter, r *http.Request) {
	si := s.info.StorageInfo()
	usage := s.info.DiskUsage(r.Context())
	resp := serverInfoResponse{
		Version:      s.version,
		DeploymentID: si.DeploymentID,
		Pools:        make([]poolInfo, 0, len(si.Pools)),
	}
	setIdx := 0
	for _, p := range si.Pools {
		pi := poolInfo{Sets: make([]setInfo, 0, len(p.Sets))}
		for _, set := range p.Sets {
			si := summarizeSet(set)
			if setIdx < len(usage.Sets) {
				applyUsage(&si, usage.Sets[setIdx])
			}
			setIdx++
			resp.DriveCount += si.DriveCount
			resp.OnlineDrives += si.OnlineCount
			resp.SetCount++
			pi.Sets = append(pi.Sets, si)
		}
		resp.Pools = append(resp.Pools, pi)
	}
	resp.PoolCount = len(resp.Pools)
	resp.RawCapacity = usage.RawTotal
	resp.RawFree = usage.RawFree
	resp.UsableCapacity = usage.UsableTotal
	resp.UsableFree = usage.UsableFree
	writeJSON(w, http.StatusOK, resp)
}

// applyUsage copies a set's capacity figures onto its wire shape.
func applyUsage(si *setInfo, su object.SetUsage) {
	si.RawCapacity = su.RawTotal
	si.RawFree = su.RawFree
	si.UsableCapacity = su.UsableTotal
	si.UsableFree = su.UsableFree
	si.DrivesReporting = su.DrivesReporting
}

// health reports the rolled-up health of every set: degraded when a drive is down,
// unavailable when a set has fallen below read quorum. It is the detailed,
// authenticated health view (distinct from an unauthenticated liveness probe), so
// it returns 200 with the status in the body for any reachable, authorized caller.
func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	resp := healthResponse{Status: "healthy"}
	for _, p := range s.info.StorageInfo().Pools {
		for _, set := range p.Sets {
			si := summarizeSet(set)
			resp.Sets++
			if !si.Available {
				resp.Unavailable++
			} else if !si.Healthy {
				resp.Degraded++
			}
		}
	}
	switch {
	case resp.Unavailable > 0:
		resp.Status = "unavailable"
	case resp.Degraded > 0:
		resp.Status = "degraded"
	}
	writeJSON(w, http.StatusOK, resp)
}

// summarizeSet maps a topology set to its wire shape and derives the counts. Read
// quorum is K = N - M; a set is healthy when every drive is online and available
// when at least K are.
func summarizeSet(set object.SetInfo) setInfo {
	si := setInfo{
		Parity:     set.Parity,
		DriveCount: len(set.Drives),
		ReadQuorum: len(set.Drives) - set.Parity,
		Drives:     make([]driveInfo, 0, len(set.Drives)),
	}
	for _, d := range set.Drives {
		if d.Online {
			si.OnlineCount++
		}
		si.Drives = append(si.Drives, driveInfo{Endpoint: d.Endpoint, Online: d.Online})
	}
	si.Healthy = si.OnlineCount == si.DriveCount
	si.Available = si.OnlineCount >= si.ReadQuorum
	return si
}
