// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"

	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
	"github.com/tamnd/liteio/storage/remote"
)

// FormatLocalDrive ensures the drive rooted at dir carries a format.json that
// agrees with want, and returns the record now on disk. It is the format-or-join
// step bring-up runs on every drive this node owns:
//
//   - If the drive has no format.json it is being formatted for the first time:
//     want is written and returned.
//   - If the drive already has one, it must agree with the deployment (same
//     deployment ID, set size, and placement algorithm — Format.Verify) and must
//     come back up at the same coordinates it left (same pool, set, and drive
//     index). A drive that disagrees is refused rather than silently re-formatted,
//     since re-formatting would discard the shards it holds.
//
// The check on coordinates is what makes a restart safe: a drive that was set 2
// drive 3 must not be accepted as set 0 drive 0, or placement would look for its
// shards in the wrong slot.
func FormatLocalDrive(dir string, want Format) (Format, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return Format{}, fmt.Errorf("cluster: prepare drive %s: %w", dir, err)
	}
	path := filepath.Join(dir, FormatFile)
	b, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return writeFormat(path, want)
	case err != nil:
		return Format{}, fmt.Errorf("cluster: read %s: %w", path, err)
	}

	have, err := ParseFormat(b)
	if err != nil {
		return Format{}, err
	}
	if err := have.Verify(want.DeploymentID, want.SetSize, want.Algorithm); err != nil {
		return Format{}, err
	}
	if have.Pool != want.Pool || have.Set != want.Set || have.DriveIndex != want.DriveIndex {
		return Format{}, fmt.Errorf("cluster: drive %s holds coordinates pool=%d set=%d drive=%d "+
			"but the layout expects pool=%d set=%d drive=%d",
			dir, have.Pool, have.Set, have.DriveIndex, want.Pool, want.Set, want.DriveIndex)
	}
	return have, nil
}

// writeFormat marshals f and writes it to path atomically (write-temp-rename) so
// a crash mid-write never leaves a torn format.json.
func writeFormat(path string, f Format) (Format, error) {
	data, err := f.Marshal()
	if err != nil {
		return Format{}, err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return Format{}, fmt.Errorf("cluster: write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return Format{}, fmt.Errorf("cluster: commit %s: %w", path, err)
	}
	return f, nil
}

// Opener turns a layout's endpoints into live drives during bring-up. A node
// opens the drives it owns on the local filesystem (writing or verifying their
// format.json) and dials every other drive over the cluster transport.
type Opener struct {
	// Local reports whether endpoint e is a drive this node serves on its own
	// filesystem. Only local drives are formatted. When Local is nil an endpoint
	// is treated as local exactly when it is a bare path (Endpoint.IsLocal), which
	// is the single-node, all-local case.
	Local func(e Endpoint) bool

	// Client is the mTLS http.Client (from cluster/mtls) used to dial remote
	// drives. A nil Client uses the transport's default, which is fine for plain
	// HTTP in tests but carries no client certificate.
	Client *http.Client
}

// isLocal applies the Local predicate, defaulting to Endpoint.IsLocal.
func (o Opener) isLocal(e Endpoint) bool {
	if o.Local == nil {
		return e.IsLocal()
	}
	return o.Local(e)
}

// open returns the drive for endpoint e. For a local endpoint it formats (or
// verifies) the drive against want and opens it on the filesystem; for a remote
// endpoint it dials the peer over the transport and ignores want, since the
// peer's own bring-up owns that drive's format.json.
func (o Opener) open(e Endpoint, want Format) (storage.StorageAPI, error) {
	if o.isLocal(e) {
		if _, err := FormatLocalDrive(e.Path, want); err != nil {
			return nil, err
		}
		return local.New(e.Path)
	}
	return remote.NewStorage(e.String(), o.Client), nil
}

// PoolSpec describes one server pool to bring up: the drive endpoints that
// compose it (as produced by ParsePattern) and its parity count M, the number of
// shards the set can lose and still serve.
type PoolSpec struct {
	Endpoints []Endpoint
	Parity    int
}

// Membership carries this node's identity and the cluster lock quorum used to
// install distributed namespace locking. NodeID is this node's name as the owner
// of the locks it takes; Lockers is the lock set a namespace lock spans (this
// node's own authority plus its peers, as built by LockQuorum). A zero
// Membership leaves the object layer at its single-node lock default, which is
// correct for a single-node deployment.
type Membership struct {
	NodeID  string
	Lockers []lock.Locker

	// CacheNotifier, when set, is installed as the object layer's cross-node
	// metacache notifier (see CacheBroadcaster), so a write here invalidates the
	// listing caches peers hold. A zero Membership leaves the layer making no
	// cross-node notifications, correct for a single-node deployment.
	CacheNotifier func(bucket, key string)
}

// BringUp computes each pool's layout, opens every drive through opener
// (formatting the local ones), and assembles the object layer over them. All
// pools share deploymentID as the placement salt. It returns the running object
// layer and the computed layouts, the latter so the caller can record the
// topology or derive the peer list.
//
// When m carries a lock quorum (m.Lockers non-empty) the object layer takes its
// namespace locks across that quorum, so concurrent mutations of one key are
// serialized cluster-wide. A zero Membership leaves the single-node in-process
// lock in place.
func BringUp(deploymentID [16]byte, specs []PoolSpec, opener Opener, m Membership) (*object.ServerPools, []Layout, error) {
	if len(specs) == 0 {
		return nil, nil, fmt.Errorf("cluster: bring-up needs at least one pool")
	}
	layouts := make([]Layout, len(specs))
	configs := make([]object.PoolConfig, len(specs))
	for pi, spec := range specs {
		layout, err := ComputeLayout(deploymentID, spec.Endpoints)
		if err != nil {
			return nil, nil, fmt.Errorf("cluster: pool %d layout: %w", pi, err)
		}
		layouts[pi] = layout

		formats := layout.Formats(pi)
		sets := make([]object.SetConfig, len(layout.Sets))
		for si, set := range layout.Sets {
			drives := make([]storage.StorageAPI, len(set))
			for di, e := range set {
				d, err := opener.open(e, formats[si][di])
				if err != nil {
					return nil, nil, fmt.Errorf("cluster: pool %d set %d drive %d (%s): %w",
						pi, si, di, e.String(), err)
				}
				drives[di] = d
			}
			sets[si] = object.SetConfig{Drives: drives, Parity: spec.Parity}
		}
		configs[pi] = object.PoolConfig{Sets: sets}
	}

	var opts []object.Option
	if len(m.Lockers) > 0 {
		opts = append(opts, object.WithLockers(m.NodeID, m.Lockers))
	}
	if m.CacheNotifier != nil {
		opts = append(opts, object.WithCacheNotifier(m.CacheNotifier))
	}
	sp, err := object.NewServerPools(deploymentID, configs, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("cluster: assemble object layer: %w", err)
	}
	return sp, layouts, nil
}
