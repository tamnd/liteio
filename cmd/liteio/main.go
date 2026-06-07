// SPDX-License-Identifier: Apache-2.0

// Command liteio runs the S3-compatible object server. It builds a single erasure
// set over a list of local drive directories and serves the S3 REST API on the
// configured address. This is the M1 single-node entrypoint; the distributed
// cluster, admin API, and console arrive with later milestones.
package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("liteio exited", "err", err)
		os.Exit(1)
	}
}

// config holds the parsed command-line configuration.
type config struct {
	address      string
	drives       string
	parity       int
	accessKey    string
	secretKey    string
	domain       string
	deploymentID string

	// Cluster mode. When clusterAddress is set the node runs distributed: it
	// serves its drives and lock authority on clusterAddress for peers to reach,
	// interprets --drives as endpoint patterns (which may name remote hosts), and
	// takes namespace locks across a quorum built from --peers. When it is empty
	// the node runs single-node over local drive directories.
	clusterAddress  string
	nodeHost        string
	peers           string
	clusterCert     string
	clusterKey      string
	clusterCA       string
	clusterServerNm string
}

func run(argv []string) error {
	cfg, err := parseFlags(argv)
	if err != nil {
		return err
	}

	layer, clusterSrv, err := buildLayer(cfg)
	if err != nil {
		return err
	}

	store := auth.NewStaticStore(auth.Credentials{AccessKey: cfg.accessKey, SecretKey: cfg.secretKey})
	var opts []s3.Option
	if cfg.domain != "" {
		opts = append(opts, s3.WithDomain(cfg.domain))
	}
	handler := s3.NewServer(layer, store, opts...)

	srv := &http.Server{
		Addr:              cfg.address,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Drain the reactive-heal queue for the life of the process, repairing objects
	// that reached quorum while a drive was briefly down.
	if sp, ok := layer.(*object.ServerPools); ok {
		sp.StartHealing(ctx)
	}

	errCh := make(chan error, 2)
	go func() {
		slog.Info("liteio listening", "address", cfg.address)
		errCh <- srv.ListenAndServe()
	}()

	// In cluster mode, also serve the inter-node surface (drives + lock endpoint)
	// so peers can reach this node. The listener is wrapped in mutual TLS when
	// cluster certificates are configured.
	var clusterHTTP *http.Server
	if clusterSrv != nil {
		clusterHTTP = &http.Server{
			Addr:              cfg.clusterAddress,
			Handler:           clusterSrv.Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		tlsCfg, terr := clusterServerTLS(cfg)
		if terr != nil {
			return terr
		}
		clusterHTTP.TLSConfig = tlsCfg
		go func() {
			slog.Info("liteio cluster listening", "address", cfg.clusterAddress, "mtls", tlsCfg != nil)
			if tlsCfg != nil {
				errCh <- clusterHTTP.ListenAndServeTLS("", "")
			} else {
				errCh <- clusterHTTP.ListenAndServe()
			}
		}()
	}

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if clusterHTTP != nil {
			_ = clusterHTTP.Shutdown(shutCtx)
		}
		return srv.Shutdown(shutCtx)
	case serr := <-errCh:
		if errors.Is(serr, http.ErrServerClosed) {
			return nil
		}
		return serr
	}
}

func parseFlags(argv []string) (config, error) {
	fs := flag.NewFlagSet("liteio", flag.ContinueOnError)
	var cfg config
	fs.StringVar(&cfg.address, "address", ":9000", "listen address")
	fs.StringVar(&cfg.drives, "drives", "", "comma-separated drive directories")
	fs.IntVar(&cfg.parity, "parity", 0, "parity shard count M (default: drives/2)")
	fs.StringVar(&cfg.accessKey, "access-key", envOr("LITEIO_ACCESS_KEY", "liteioadmin"), "root access key")
	fs.StringVar(&cfg.secretKey, "secret-key", envOr("LITEIO_SECRET_KEY", "liteioadmin"), "root secret key")
	fs.StringVar(&cfg.domain, "domain", os.Getenv("LITEIO_DOMAIN"), "base domain for virtual-host addressing")
	fs.StringVar(&cfg.deploymentID, "deployment-id", envOr("LITEIO_DEPLOYMENT_ID", "liteio-default"), "placement salt")
	fs.StringVar(&cfg.clusterAddress, "cluster-address", os.Getenv("LITEIO_CLUSTER_ADDRESS"), "inter-node listen address; enables distributed mode")
	fs.StringVar(&cfg.nodeHost, "node-host", os.Getenv("LITEIO_NODE_HOST"), "this node's host as it appears in --drives endpoints (cluster mode)")
	fs.StringVar(&cfg.peers, "peers", os.Getenv("LITEIO_PEERS"), "comma-separated peer base URLs for the lock quorum (cluster mode)")
	fs.StringVar(&cfg.clusterCert, "cluster-cert", os.Getenv("LITEIO_CLUSTER_CERT"), "node certificate for inter-node mTLS (cluster mode)")
	fs.StringVar(&cfg.clusterKey, "cluster-key", os.Getenv("LITEIO_CLUSTER_KEY"), "node private key for inter-node mTLS (cluster mode)")
	fs.StringVar(&cfg.clusterCA, "cluster-ca", os.Getenv("LITEIO_CLUSTER_CA"), "cluster CA bundle for inter-node mTLS (cluster mode)")
	fs.StringVar(&cfg.clusterServerNm, "cluster-server-name", os.Getenv("LITEIO_CLUSTER_SERVER_NAME"), "SAN the peer certificates must carry (cluster mTLS)")
	if err := fs.Parse(argv); err != nil {
		return config{}, err
	}
	if cfg.parity == 0 {
		cfg.parity = max(1, len(splitNonEmpty(cfg.drives))/2)
	}
	return cfg, nil
}

// deploymentSalt derives the 16-byte placement salt from the deployment ID
// string by hashing it, so any human-readable ID yields a stable salt.
func deploymentSalt(id string) [16]byte {
	sum := sha256.Sum256([]byte(id))
	var out [16]byte
	copy(out[:], sum[:16])
	return out
}

func splitNonEmpty(s string) []string {
	var out []string
	for p := range strings.SplitSeq(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
