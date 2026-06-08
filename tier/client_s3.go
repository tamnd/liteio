// SPDX-License-Identifier: Apache-2.0

package tier

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
)

// s3Client is a minimal S3-compatible client for PUT/GET/DELETE operations against
// a remote tier. It signs requests with SigV4 using the credentials in S3Config.
type s3Client struct {
	cfg    S3Config
	cred   auth.Credentials
	client *http.Client
}

func newS3Client(cfg S3Config) *s3Client {
	return &s3Client{
		cfg:    cfg,
		cred:   auth.Credentials{AccessKey: cfg.AccessKey, SecretKey: cfg.SecretKey},
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

func (c *s3Client) objectURL(key string) string {
	ep := strings.TrimSuffix(c.cfg.Endpoint, "/")
	return ep + "/" + c.cfg.Bucket + "/" + key
}

// Put implements RemoteClient: PUTs body (size bytes) at key in the remote bucket.
func (c *s3Client) Put(ctx context.Context, key string, body io.Reader, size int64) error {
	url := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("tier/s3: build PUT: %w", err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	sign.SignHeader(req, c.cred, c.cfg.Region, sign.UnsignedPayload, time.Now().UTC())
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("tier/s3: PUT %s: %w", key, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("tier/s3: PUT %s returned %d", key, resp.StatusCode)
	}
	return nil
}

// Get implements RemoteClient: GETs key from the remote bucket. The caller must
// close the returned reader to release the underlying HTTP connection.
func (c *s3Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	url := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("tier/s3: build GET: %w", err)
	}
	sign.SignHeader(req, c.cred, c.cfg.Region, sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("tier/s3: GET %s: %w", key, err)
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("tier/s3: GET %s: not found", key)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, 0, fmt.Errorf("tier/s3: GET %s returned %d", key, resp.StatusCode)
	}
	size, _ := strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	return resp.Body, size, nil
}

// Remove implements RemoteClient: DELETEs key from the remote bucket.
func (c *s3Client) Remove(ctx context.Context, key string) error {
	url := c.objectURL(key)
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("tier/s3: build DELETE: %w", err)
	}
	sign.SignHeader(req, c.cred, c.cfg.Region, sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("tier/s3: DELETE %s: %w", key, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("tier/s3: DELETE %s returned %d", key, resp.StatusCode)
	}
	return nil
}
