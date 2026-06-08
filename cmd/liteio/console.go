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
)

// buildConsole assembles the listener that serves both the admin REST API and the
// web console over one identity store. The admin API answers SigV4-signed calls at
// its path prefix (the surface a command-line admin tool drives); the console
// serves the browser app at every other path and signs its own calls into the very
// same admin handler in-process, so the browser never holds a secret key. The
// returned *console.Server is the seam the caller sweeps expired sessions on.
func buildConsole(cfg config, store *auth.Store) (http.Handler, *console.Server, error) {
	adminSrv := admin.NewServer(store, store)

	opts := []console.Option{}
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
