// SPDX-License-Identifier: Apache-2.0

// Command liteio runs the S3-compatible object server. It builds an erasure set
// over a list of local drive directories (or, in cluster mode, over endpoints that
// may name remote hosts) and serves the S3 REST API on the configured address.
// Alongside it the node serves the STS endpoint at the service root and, on a
// second listener, the admin REST API and the web console, all backed by one IAM
// identity store seeded with the root credential.
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
	"github.com/tamnd/liteio/metrics"
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

	// Admin and console. consoleAddress is the listener that serves the admin REST
	// API and the web console; empty disables both. consoleRegion overrides the
	// region the console signs admin calls under (empty keeps the default). When
	// consoleInsecureCookie is set the session cookie drops its Secure attribute so
	// the console works over plain HTTP for local testing; never set it in
	// production, where the console must sit behind TLS.
	consoleAddress        string
	consoleRegion         string
	consoleInsecureCookie bool

	// metricsToken gates the Prometheus metrics endpoint (doc 10.4): when set, the
	// console listener serves /metrics to a scraper that presents this token as a
	// bearer credential. Empty disables the endpoint, so metrics are never exposed
	// without an explicit token.
	metricsToken string
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

	// One identity store backs every authenticated surface: it verifies SigV4
	// signatures, decides policy, issues and validates STS sessions, and answers the
	// admin API. The store is seeded with the root credential; further users are
	// created through the admin API or console.
	store := auth.NewStore(cfg.accessKey, cfg.secretKey)
	// One registry collects the front-door metrics and is scraped through the
	// token-gated /metrics endpoint on the console listener (doc 10.4).
	registry := metrics.NewRegistry()
	opts := []s3.Option{
		s3.WithAuthorizer(store),
		s3.WithSTS(store),
		s3.WithSessionValidator(store),
		s3.WithMetrics(registry),
	}
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

	errCh := make(chan error, 3)
	go func() {
		slog.Info("liteio listening", "address", cfg.address)
		errCh <- srv.ListenAndServe()
	}()

	// Serve the admin REST API and the web console on their own listener, sweeping
	// expired console sessions in the background. Disabled when no address is set.
	var consoleHTTP *http.Server
	if cfg.consoleAddress != "" {
		consoleHandler, csrv, cerr := buildConsole(cfg, store, layer, handler, registry)
		if cerr != nil {
			return cerr
		}
		consoleHTTP = &http.Server{
			Addr:              cfg.consoleAddress,
			Handler:           consoleHandler,
			ReadHeaderTimeout: 10 * time.Second,
		}
		startSweeping(ctx, csrv)
		go func() {
			slog.Info("liteio console listening", "address", cfg.consoleAddress, "insecure_cookie", cfg.consoleInsecureCookie)
			errCh <- consoleHTTP.ListenAndServe()
		}()
	}

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
		if consoleHTTP != nil {
			_ = consoleHTTP.Shutdown(shutCtx)
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
	fs.StringVar(&cfg.consoleAddress, "console-address", envOr("LITEIO_CONSOLE_ADDRESS", ":9001"), "listen address for the admin API and web console; empty disables them")
	fs.StringVar(&cfg.consoleRegion, "console-region", os.Getenv("LITEIO_CONSOLE_REGION"), "region the console signs admin calls under (default: us-east-1)")
	fs.BoolVar(&cfg.consoleInsecureCookie, "console-insecure-cookie", os.Getenv("LITEIO_CONSOLE_INSECURE_COOKIE") == "1", "drop the Secure attribute on the console cookie for plain-HTTP local testing")
	fs.StringVar(&cfg.metricsToken, "metrics-token", os.Getenv("LITEIO_METRICS_TOKEN"), "bearer token gating the Prometheus /metrics endpoint on the console listener; empty disables it")
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
