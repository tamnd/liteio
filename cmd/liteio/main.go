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
	"fmt"
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
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
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
}

func run(argv []string) error {
	cfg, err := parseFlags(argv)
	if err != nil {
		return err
	}

	driveDirs := splitNonEmpty(cfg.drives)
	if len(driveDirs) < 2 {
		return errors.New("liteio: at least 2 drives are required (--drives dir1,dir2,...)")
	}
	if cfg.parity < 1 || cfg.parity >= len(driveDirs) {
		return fmt.Errorf("liteio: parity %d out of range for %d drives", cfg.parity, len(driveDirs))
	}

	drives := make([]storage.StorageAPI, 0, len(driveDirs))
	for _, dir := range driveDirs {
		d, derr := local.New(dir)
		if derr != nil {
			return fmt.Errorf("liteio: open drive %q: %w", dir, derr)
		}
		drives = append(drives, d)
	}

	layer, err := object.NewSingleSet(deploymentSalt(cfg.deploymentID), drives, cfg.parity)
	if err != nil {
		return fmt.Errorf("liteio: build object layer: %w", err)
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

	errCh := make(chan error, 1)
	go func() {
		slog.Info("liteio listening", "address", cfg.address, "drives", len(drives), "parity", cfg.parity)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		slog.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
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
