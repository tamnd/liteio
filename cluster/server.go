// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/cluster/rpc"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/remote"
)

// LockPath is the well-known URL path under which every node serves its lock
// endpoint. A peer reaches it by dialing scheme://host + LockPath. Drive
// endpoints carry their own per-drive paths from the deployment pattern, so a
// drive path must never equal LockPath.
const LockPath = "/liteio-lock"

// Server is a node's inbound half of the cluster: it serves that node's local
// drives and its lock authority to peers over cluster/rpc. Each drive is mounted
// under its own endpoint path and the lock endpoint under LockPath, all on one
// HTTP handler so a single listener (wrapped in mutual TLS) carries the whole
// inter-node surface.
//
// The Server is transport-secured by the caller: hand Handler to an http.Server
// configured with cluster/mtls.ServerConfig. The Server itself is unaware of
// TLS, mirroring how cluster/rpc and the lock service stay transport-ignorant.
type Server struct {
	mux *http.ServeMux
}

// NewServer builds a node server. drives maps each local drive's endpoint path
// (the Path of the Endpoint that names it, for example "/mnt/disk1") to the
// StorageAPI serving it; locker is the node's lock authority (a
// lock.LocalLocker), served at LockPath. Drive paths must be non-empty, distinct,
// and different from LockPath.
func NewServer(drives map[string]storage.StorageAPI, locker lock.Locker) (*Server, error) {
	if locker == nil {
		return nil, fmt.Errorf("cluster: node server needs a lock authority")
	}
	mux := http.NewServeMux()
	for path, drive := range drives {
		if path == "" || path == "/" {
			return nil, fmt.Errorf("cluster: drive endpoint path %q is not usable", path)
		}
		if path == LockPath {
			return nil, fmt.Errorf("cluster: drive endpoint path %q collides with the lock endpoint", path)
		}
		dmux := rpc.NewMux()
		remote.Register(dmux, drive)
		// Strip the drive prefix so the drive's mux sees the bare rpc path it
		// registered under; the prefix is how peers address this drive.
		mux.Handle(path+"/", http.StripPrefix(path, dmux))
	}

	lmux := rpc.NewMux()
	lock.Register(lmux, locker)
	mux.Handle(LockPath+"/", http.StripPrefix(LockPath, lmux))

	return &Server{mux: mux}, nil
}

// Handler returns the node's HTTP handler, ready to serve over an http.Server
// configured with cluster/mtls.ServerConfig.
func (s *Server) Handler() http.Handler { return s.mux }

// LockQuorum builds the namespace-lock quorum from one node's point of view:
// this node's own locker plus a RemoteLocker for each peer's lock endpoint. The
// result is the []lock.Locker a DRWMutex spans, so a lock is held only when a
// majority of the nodes grant it. peers are the peers' base URLs
// (scheme://host); client is the mutual-TLS client used to reach them.
//
// self may be nil for a node that serves no lock authority of its own, though in
// practice every node runs one so the quorum counts an odd number of voters.
func LockQuorum(self lock.Locker, peers []string, client *http.Client) []lock.Locker {
	q := make([]lock.Locker, 0, len(peers)+1)
	if self != nil {
		q = append(q, self)
	}
	for _, base := range peers {
		endpoint := strings.TrimRight(base, "/") + LockPath
		q = append(q, lock.NewRemoteLocker(endpoint, client))
	}
	return q
}
