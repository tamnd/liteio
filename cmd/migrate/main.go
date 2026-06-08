// SPDX-License-Identifier: Apache-2.0

// Command migrate copies all objects from a MinIO (or any S3-compatible)
// endpoint to a liteio endpoint. It enumerates every bucket from the source,
// lists all objects, and streams each one with a SigV4-signed GET followed by a
// SigV4-signed PUT to the destination. Both sides speak the standard S3 wire
// protocol so any pair of S3-compatible endpoints works.
//
// Usage:
//
//	migrate \
//	  --src  http://minio:9000        \
//	  --src-ak MINIO_ACCESS_KEY      \
//	  --src-sk MINIO_SECRET_KEY      \
//	  --dst  http://liteio:9000       \
//	  --dst-ak LITEIO_ACCESS_KEY     \
//	  --dst-sk LITEIO_SECRET_KEY     \
//	  [--bucket BUCKET]              \
//	  [--workers 8]                  \
//	  [--region us-east-1]
package main

import (
	"context"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		slog.Error("migrate", "err", err)
		os.Exit(1)
	}
}

type cfg struct {
	srcEndpoint, srcAK, srcSK string
	dstEndpoint, dstAK, dstSK string
	bucket                    string
	workers                   int
	region                    string
}

func run(argv []string) error {
	var c cfg
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.StringVar(&c.srcEndpoint, "src", "", "source S3 endpoint URL (required)")
	fs.StringVar(&c.srcAK, "src-ak", "", "source access key (required)")
	fs.StringVar(&c.srcSK, "src-sk", "", "source secret key (required)")
	fs.StringVar(&c.dstEndpoint, "dst", "", "destination S3 endpoint URL (required)")
	fs.StringVar(&c.dstAK, "dst-ak", "", "destination access key (required)")
	fs.StringVar(&c.dstSK, "dst-sk", "", "destination secret key (required)")
	fs.StringVar(&c.bucket, "bucket", "", "migrate only this bucket (empty = all buckets)")
	fs.IntVar(&c.workers, "workers", 8, "concurrent copy workers")
	fs.StringVar(&c.region, "region", "us-east-1", "AWS region for SigV4")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	for _, f := range []struct{ name, val string }{
		{"--src", c.srcEndpoint}, {"--src-ak", c.srcAK}, {"--src-sk", c.srcSK},
		{"--dst", c.dstEndpoint}, {"--dst-ak", c.dstAK}, {"--dst-sk", c.dstSK},
	} {
		if f.val == "" {
			return fmt.Errorf("%s is required", f.name)
		}
	}
	m := &migrator{cfg: c, hc: &http.Client{Timeout: 5 * time.Minute}}
	return m.migrate(context.Background())
}

type migrator struct {
	cfg cfg
	hc  *http.Client
}

func (m *migrator) srcCreds() auth.Credentials {
	return auth.Credentials{AccessKey: m.cfg.srcAK, SecretKey: m.cfg.srcSK}
}

func (m *migrator) dstCreds() auth.Credentials {
	return auth.Credentials{AccessKey: m.cfg.dstAK, SecretKey: m.cfg.dstSK}
}

func (m *migrator) migrate(ctx context.Context) error {
	var buckets []string
	if m.cfg.bucket != "" {
		buckets = []string{m.cfg.bucket}
	} else {
		var err error
		buckets, err = m.listBuckets(ctx)
		if err != nil {
			return fmt.Errorf("list source buckets: %w", err)
		}
	}
	for _, b := range buckets {
		slog.Info("migrating bucket", "bucket", b)
		if err := m.migrateBucket(ctx, b); err != nil {
			slog.Error("bucket failed", "bucket", b, "err", err)
		}
	}
	return nil
}

func (m *migrator) migrateBucket(ctx context.Context, bucket string) error {
	if err := m.ensureBucket(ctx, bucket); err != nil {
		return err
	}
	keys, err := m.listObjects(ctx, bucket)
	if err != nil {
		return err
	}
	slog.Info("objects found", "bucket", bucket, "count", len(keys))

	work := make(chan string, len(keys))
	for _, k := range keys {
		work <- k
	}
	close(work)

	var wg sync.WaitGroup
	errs := make(chan error, len(keys))
	for w := 0; w < m.cfg.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for key := range work {
				if err := m.copyObject(ctx, bucket, key); err != nil {
					slog.Error("copy failed", "bucket", bucket, "key", key, "err", err)
					errs <- err
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	var firstErr error
	for e := range errs {
		if firstErr == nil {
			firstErr = e
		}
	}
	return firstErr
}

func (m *migrator) listBuckets(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", m.cfg.srcEndpoint+"/", nil)
	if err != nil {
		return nil, err
	}
	sign.SignHeader(req, m.srcCreds(), m.cfg.region, sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := m.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("list buckets: status %d", resp.StatusCode)
	}
	var out struct {
		Buckets struct {
			Bucket []struct {
				Name string `xml:"Name"`
			} `xml:"Bucket"`
		} `xml:"Buckets"`
	}
	if err := xml.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, len(out.Buckets.Bucket))
	for i, b := range out.Buckets.Bucket {
		names[i] = b.Name
	}
	return names, nil
}

func (m *migrator) listObjects(ctx context.Context, bucket string) ([]string, error) {
	var keys []string
	token := ""
	for {
		u := m.cfg.srcEndpoint + "/" + bucket + "?list-type=2&max-keys=1000"
		if token != "" {
			u += "&continuation-token=" + token
		}
		req, err := http.NewRequestWithContext(ctx, "GET", u, nil)
		if err != nil {
			return nil, err
		}
		sign.SignHeader(req, m.srcCreds(), m.cfg.region, sign.EmptyPayloadHash, time.Now().UTC())
		resp, err := m.hc.Do(req)
		if err != nil {
			return nil, err
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, fmt.Errorf("list objects %s: status %d", bucket, resp.StatusCode)
		}
		var page struct {
			Contents []struct {
				Key string `xml:"Key"`
			} `xml:"Contents"`
			Truncated bool   `xml:"IsTruncated"`
			NextToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		for _, c := range page.Contents {
			keys = append(keys, c.Key)
		}
		if !page.Truncated {
			break
		}
		token = page.NextToken
	}
	return keys, nil
}

func (m *migrator) ensureBucket(ctx context.Context, bucket string) error {
	req, err := http.NewRequestWithContext(ctx, "PUT", m.cfg.dstEndpoint+"/"+bucket, nil)
	if err != nil {
		return err
	}
	sign.SignHeader(req, m.dstCreds(), m.cfg.region, sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := m.hc.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	// 200 = created; 409 = already exists — both acceptable.
	if resp.StatusCode != 200 && resp.StatusCode != 409 {
		return fmt.Errorf("create bucket %s: status %d", bucket, resp.StatusCode)
	}
	return nil
}

func (m *migrator) copyObject(ctx context.Context, bucket, key string) error {
	// GET from source.
	srcURL := m.cfg.srcEndpoint + "/" + bucket + "/" + strings.TrimPrefix(key, "/")
	getReq, err := http.NewRequestWithContext(ctx, "GET", srcURL, nil)
	if err != nil {
		return err
	}
	sign.SignHeader(getReq, m.srcCreds(), m.cfg.region, sign.EmptyPayloadHash, time.Now().UTC())
	getResp, err := m.hc.Do(getReq)
	if err != nil {
		return err
	}
	if getResp.StatusCode != 200 {
		_ = getResp.Body.Close()
		return fmt.Errorf("GET %s/%s: status %d", bucket, key, getResp.StatusCode)
	}
	defer func() { _ = getResp.Body.Close() }()

	// PUT to destination with the same content-type.
	dstURL := m.cfg.dstEndpoint + "/" + bucket + "/" + strings.TrimPrefix(key, "/")
	putReq, err := http.NewRequestWithContext(ctx, "PUT", dstURL, getResp.Body)
	if err != nil {
		return err
	}
	putReq.ContentLength = getResp.ContentLength
	if ct := getResp.Header.Get("Content-Type"); ct != "" {
		putReq.Header.Set("Content-Type", ct)
	}
	sign.SignHeader(putReq, m.dstCreds(), m.cfg.region, sign.UnsignedPayload, time.Now().UTC())
	putResp, err := m.hc.Do(putReq)
	if err != nil {
		return err
	}
	_ = putResp.Body.Close()
	if putResp.StatusCode != 200 {
		return fmt.Errorf("PUT %s/%s: status %d", bucket, key, putResp.StatusCode)
	}
	slog.Info("copied", "bucket", bucket, "key", key)
	return nil
}
