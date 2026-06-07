// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net/http"

	"github.com/tamnd/liteio/cluster"
	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/cluster/mtls"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// buildLayer assembles the object layer the S3 front door serves. In single-node
// mode (no --cluster-address) it opens the local drive directories and builds one
// erasure set, and the returned cluster server is nil. In cluster mode it brings
// up the deployment from endpoint patterns through cluster.BringUp, installs a
// namespace-lock quorum over its peers, and returns the inter-node server the
// caller must also listen on so peers can reach this node.
func buildLayer(cfg config) (object.ObjectLayer, *cluster.Server, error) {
	if cfg.clusterAddress == "" {
		layer, err := buildSingleNode(cfg)
		return layer, nil, err
	}
	return buildCluster(cfg)
}

// buildSingleNode opens the local drive directories and builds one erasure set.
func buildSingleNode(cfg config) (object.ObjectLayer, error) {
	driveDirs := splitNonEmpty(cfg.drives)
	if len(driveDirs) < 2 {
		return nil, errors.New("liteio: at least 2 drives are required (--drives dir1,dir2,...)")
	}
	if cfg.parity < 1 || cfg.parity >= len(driveDirs) {
		return nil, fmt.Errorf("liteio: parity %d out of range for %d drives", cfg.parity, len(driveDirs))
	}
	drives := make([]storage.StorageAPI, 0, len(driveDirs))
	for _, dir := range driveDirs {
		d, err := local.New(dir)
		if err != nil {
			return nil, fmt.Errorf("liteio: open drive %q: %w", dir, err)
		}
		drives = append(drives, d)
	}
	layer, err := object.NewSingleSet(deploymentSalt(cfg.deploymentID), drives, cfg.parity)
	if err != nil {
		return nil, fmt.Errorf("liteio: build object layer: %w", err)
	}
	return layer, nil
}

// buildCluster brings up a distributed node. It parses --drives as endpoint
// patterns (which may name remote hosts), dials remote drives and peer lock
// endpoints over the inter-node client, formats the drives this node owns, and
// installs a namespace-lock quorum across this node plus its peers.
//
// A drive endpoint is local to this node when it is a bare path or when its host
// equals --node-host. The node's own lock authority is the very LocalLocker its
// inter-node server serves, so the quorum this node builds and the quorum its
// peers reach through it span the same authorities — the invariant distributed
// mutual exclusion depends on.
func buildCluster(cfg config) (object.ObjectLayer, *cluster.Server, error) {
	if cfg.nodeHost == "" {
		return nil, nil, errors.New("liteio: cluster mode requires --node-host")
	}
	patterns := splitNonEmpty(cfg.drives)
	if len(patterns) == 0 {
		return nil, nil, errors.New("liteio: cluster mode requires --drives endpoint patterns")
	}
	endpoints, err := cluster.ParsePattern(patterns)
	if err != nil {
		return nil, nil, fmt.Errorf("liteio: parse drive endpoints: %w", err)
	}
	if cfg.parity < 1 {
		return nil, nil, fmt.Errorf("liteio: parity %d must be at least 1", cfg.parity)
	}

	client, err := clusterClient(cfg)
	if err != nil {
		return nil, nil, err
	}

	isLocal := func(e cluster.Endpoint) bool { return e.IsLocal() || e.Host == cfg.nodeHost }

	// Open the drives this node owns up front so a node that owns none fails
	// before it dials any peer, and so the same handles serve peers below.
	localDrives := make(map[string]storage.StorageAPI)
	for _, e := range endpoints {
		if !isLocal(e) {
			continue
		}
		d, derr := local.New(e.Path)
		if derr != nil {
			return nil, nil, fmt.Errorf("liteio: open local drive %q: %w", e.Path, derr)
		}
		localDrives[e.Path] = d
	}
	if len(localDrives) == 0 {
		return nil, nil, fmt.Errorf("liteio: no drive endpoint is local to node %q", cfg.nodeHost)
	}

	// The self locker must be the one the inter-node server serves below, so the
	// quorum this node builds and the quorum peers reach through it agree.
	self := lock.NewLocalLocker(cfg.nodeHost)
	peers := splitNonEmpty(cfg.peers)
	quorum := cluster.LockQuorum(self, peers, client)

	opener := cluster.Opener{Local: isLocal, Client: client}
	sp, _, err := cluster.BringUp(
		deploymentSalt(cfg.deploymentID),
		[]cluster.PoolSpec{{Endpoints: endpoints, Parity: cfg.parity}},
		opener,
		cluster.Membership{
			NodeID:        cfg.nodeHost,
			Lockers:       quorum,
			CacheNotifier: cluster.CacheBroadcaster(peers, client),
		},
	)
	if err != nil {
		return nil, nil, err
	}

	srv, err := cluster.NewServer(localDrives, self, sp)
	if err != nil {
		return nil, nil, fmt.Errorf("liteio: build node server: %w", err)
	}
	return sp, srv, nil
}

// clusterClient builds the HTTP client used to dial remote drives and peer lock
// endpoints. With cluster certificates configured it is a mutual-TLS client;
// otherwise it is the default client (plain HTTP, suitable for a trusted
// network or local testing).
func clusterClient(cfg config) (*http.Client, error) {
	if !clusterTLSConfigured(cfg) {
		return http.DefaultClient, nil
	}
	client, err := mtls.NewClient(cfg.clusterCert, cfg.clusterKey, cfg.clusterCA, cfg.clusterServerNm)
	if err != nil {
		return nil, fmt.Errorf("liteio: cluster client TLS: %w", err)
	}
	return client, nil
}

// clusterServerTLS builds the inter-node listener's TLS config, or nil when no
// cluster certificates are configured (plain HTTP).
func clusterServerTLS(cfg config) (*tls.Config, error) {
	if !clusterTLSConfigured(cfg) {
		return nil, nil
	}
	tlsCfg, err := mtls.ServerConfig(cfg.clusterCert, cfg.clusterKey, cfg.clusterCA)
	if err != nil {
		return nil, fmt.Errorf("liteio: cluster server TLS: %w", err)
	}
	return tlsCfg, nil
}

// clusterTLSConfigured reports whether the inter-node mTLS material is set.
func clusterTLSConfigured(cfg config) bool {
	return cfg.clusterCert != "" && cfg.clusterKey != "" && cfg.clusterCA != ""
}
