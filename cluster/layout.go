// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"crypto/rand"
	"fmt"
	"slices"
	"sort"

	"github.com/tamnd/liteio/object/placement"
)

// setSizeChoices lists the candidate erasure-set sizes in descending preference.
// liteio chooses the largest size that evenly divides a pool's drive count, the
// same family MinIO settled on; 7 is omitted because no common drive count makes
// it the largest divisor and it leaves awkward parity choices.
var setSizeChoices = []int{16, 15, 14, 13, 12, 11, 10, 9, 8, 6, 5, 4}

// SetSize returns the erasure-set size for a pool of total drives: the largest
// value in setSizeChoices that divides total evenly. A deployment must have at
// least four drives so a set can tolerate the loss of a drive while keeping
// write quorum, so totals below four are rejected. A total that no candidate
// divides (for example a prime above 16) is rejected rather than silently
// rounded, since an uneven split would leave a set short of drives.
func SetSize(total int) (int, error) {
	if total < 4 {
		return 0, fmt.Errorf("cluster: %d drives is too few; a pool needs at least 4", total)
	}
	for _, n := range setSizeChoices {
		if total%n == 0 {
			return n, nil
		}
	}
	return 0, fmt.Errorf("cluster: %d drives cannot be split into sets of size 4..16; "+
		"choose a count divisible by one of those", total)
}

// SetSizeWith returns sets of the operator-chosen size want for a pool of total
// drives, validating that want is one of the supported sizes and divides total
// evenly. It lets an operator override the automatic choice within the same
// constraint the layout depends on.
func SetSizeWith(total, want int) (int, error) {
	if total%want != 0 {
		return 0, fmt.Errorf("cluster: chosen set size %d does not divide %d drives", want, total)
	}
	if !slices.Contains(setSizeChoices, want) {
		return 0, fmt.Errorf("cluster: set size %d is not supported (must be one of 4..16)", want)
	}
	return want, nil
}

// Layout is one server pool's computed topology: its deployment-wide identity,
// the chosen set size, and the ordered erasure sets, each a slice of the drive
// endpoints that compose it. The position of an endpoint within Sets[i] is that
// drive's stable position in its set, recorded in its format.json.
type Layout struct {
	DeploymentID [16]byte
	SetSize      int
	Sets         [][]Endpoint
}

// NewDeploymentID returns a fresh random 16-byte deployment ID, used as the
// placement salt and frozen for the life of the deployment.
func NewDeploymentID() ([16]byte, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return id, fmt.Errorf("cluster: generate deployment ID: %w", err)
	}
	return id, nil
}

// ComputeLayout groups a pool's endpoints into erasure sets, choosing the set
// size automatically and assigning drives to maximize node spread: consecutive
// members of a set land on different hosts wherever the host count allows, so a
// set's parity tolerance maps onto distinct failure domains rather than several
// drives of one machine. The endpoints are taken in the order ParsePattern
// produced them.
func ComputeLayout(deploymentID [16]byte, endpoints []Endpoint) (Layout, error) {
	return computeLayout(deploymentID, endpoints, 0)
}

// ComputeLayoutWith is ComputeLayout with an operator-chosen set size.
func ComputeLayoutWith(deploymentID [16]byte, endpoints []Endpoint, setSize int) (Layout, error) {
	return computeLayout(deploymentID, endpoints, setSize)
}

func computeLayout(deploymentID [16]byte, endpoints []Endpoint, want int) (Layout, error) {
	total := len(endpoints)
	var (
		n   int
		err error
	)
	if want == 0 {
		n, err = SetSize(total)
	} else {
		n, err = SetSizeWith(total, want)
	}
	if err != nil {
		return Layout{}, err
	}

	ordered := spreadByHost(endpoints)
	numSets := total / n
	sets := make([][]Endpoint, numSets)
	// Cut the host-spread sequence into contiguous blocks of n. Because the
	// sequence already visits hosts round-robin, a block of n consecutive members
	// draws from the hosts in rotation, so each set is spread evenly across the
	// failure domains. Dealing round-robin instead would alias with the host
	// rotation and stack whole hosts into the same set.
	for s := range sets {
		sets[s] = ordered[s*n : (s+1)*n]
	}
	return Layout{DeploymentID: deploymentID, SetSize: n, Sets: sets}, nil
}

// spreadByHost reorders endpoints so that walking the result visits a different
// host on each step for as long as hosts remain, by round-robin draining the
// per-host queues largest-first. Local endpoints (empty Host) are treated as one
// "local" failure domain. The relative order of drives within a host is
// preserved. This is the sequence the layout deals into sets.
func spreadByHost(endpoints []Endpoint) []Endpoint {
	type bucket struct {
		host   string
		drives []Endpoint
	}
	index := map[string]int{}
	var buckets []*bucket
	for _, e := range endpoints {
		h := e.Host // "" groups all local drives together
		i, ok := index[h]
		if !ok {
			i = len(buckets)
			index[h] = i
			buckets = append(buckets, &bucket{host: h})
		}
		buckets[i].drives = append(buckets[i].drives, e)
	}
	// Stable order: by descending drive count, then by host name, so the result is
	// deterministic regardless of map iteration order.
	sort.SliceStable(buckets, func(a, b int) bool {
		if len(buckets[a].drives) != len(buckets[b].drives) {
			return len(buckets[a].drives) > len(buckets[b].drives)
		}
		return buckets[a].host < buckets[b].host
	})

	out := make([]Endpoint, 0, len(endpoints))
	for {
		progressed := false
		for _, bk := range buckets {
			if len(bk.drives) == 0 {
				continue
			}
			out = append(out, bk.drives[0])
			bk.drives = bk.drives[1:]
			progressed = true
		}
		if !progressed {
			break
		}
	}
	return out
}

// Formats returns the format.json records for every drive in the layout, indexed
// the same way as l.Sets: Formats(pool)[s][d] is the record for Sets[s][d]. The
// pool index is the layout's position in the cluster's ordered pool list.
func (l Layout) Formats(pool int) [][]Format {
	out := make([][]Format, len(l.Sets))
	for s, set := range l.Sets {
		out[s] = make([]Format, len(set))
		for d := range set {
			out[s][d] = Format{
				Version:      FormatVersion,
				DeploymentID: formatUUID(l.DeploymentID),
				Pool:         pool,
				Set:          s,
				DriveIndex:   d,
				SetSize:      l.SetSize,
				Algorithm:    placement.AlgorithmVersion,
			}
		}
	}
	return out
}
