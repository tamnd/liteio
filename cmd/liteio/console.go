// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/tamnd/liteio/admin"
	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/console"
	"github.com/tamnd/liteio/metrics"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/trace"
)

// version is the build version reported by the admin info endpoint and the console
// dashboard. It is overridden at build time with -ldflags "-X main.version=...".
var version = "dev"

// buildConsole assembles the listener that serves both the admin REST API and the
// web console over one identity store. The admin API answers SigV4-signed calls at
// its path prefix (the surface a command-line admin tool drives); the console
// serves the browser app at every other path and signs its own calls into the very
// same admin handler in-process, so the browser never holds a secret key. The
// returned *console.Server is the seam the caller sweeps expired sessions on. When a
// metrics token is configured, the listener also serves the token-gated Prometheus
// /metrics endpoint over the shared registry (doc 10.4).
func buildConsole(cfg config, store *auth.Store, layer object.ObjectLayer, s3Handler http.Handler, registry *metrics.Registry) (http.Handler, *console.Server, error) {
	adminOpts := []admin.Option{admin.WithVersion(version)}
	// The object layer reports topology and drive health for the info endpoints; a
	// layer that does not satisfy InfoSource (none does today besides ServerPools)
	// simply leaves those routes unregistered.
	if src, ok := layer.(admin.InfoSource); ok {
		adminOpts = append(adminOpts, admin.WithInfo(src))
	}
	// Wire heal status and live trace streaming when the layer is a ServerPools.
	// Both run on every node that owns an object layer; a pure control node skips
	// these (the handlers return 501 when the option is absent).
	ring := trace.NewRingBuf()
	trace.SetGlobalEmitter(ring)
	adminOpts = append(adminOpts, admin.WithTraceSource(ring))
	if sp, ok := layer.(*object.ServerPools); ok {
		adminOpts = append(adminOpts, admin.WithHealer(sp))
	}
	// Seed the cluster config with deployment defaults. An operator can update
	// these at runtime via PUT /liteio/admin/v1/config.
	cfgStore := admin.NewInMemoryConfigStore(admin.ClusterConfig{
		Region:                "us-east-1",
		MaxConcurrentRequests: 0,
		HealWorkers:           4,
		ScannerInterval:       "1h",
	})
	adminOpts = append(adminOpts, admin.WithConfig(cfgStore))
	adminSrv := admin.NewServer(store, store, adminOpts...)

	// Wire the S3 data plane into the console so the bucket browser can list buckets
	// and browse objects through the signed bridge, against the same handler the S3
	// clients use.
	opts := []console.Option{console.WithS3(s3Handler)}
	if cfg.consoleRegion != "" {
		opts = append(opts, console.WithRegion(cfg.consoleRegion))
	}
	if cfg.consoleInsecureCookie {
		opts = append(opts, console.WithInsecureCookie())
	}
	consoleSrv, err := console.NewServer(store, store, adminSrv, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("liteio: build console: %w", err)
	}

	// Route the admin path prefix straight to the admin handler so a CLI can reach
	// it with its own signature; everything else is the console app. The console
	// bridge calls adminSrv in-process and does not depend on this mux.
	mux := http.NewServeMux()
	mux.Handle(admin.APIPrefix+"/", adminSrv)
	// The metrics endpoint sits on the console listener (not the public S3 port, where
	// /metrics would collide with a bucket of that name) and is gated by a bearer
	// token; with no token configured it is not mounted at all.
	if registry != nil && cfg.metricsToken != "" {
		mux.Handle("/metrics", metricsHandler(registry, cfg.metricsToken))
	}
	mux.Handle("/", consoleSrv)
	return mux, consoleSrv, nil
}

// startSweeping drops expired console sessions on a ticker for the life of the
// process, so a long-running node does not accumulate dead session records.
func startSweeping(ctx context.Context, srv *console.Server) {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				srv.Sweep()
			}
		}
	}()
}
