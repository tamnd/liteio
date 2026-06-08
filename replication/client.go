// SPDX-License-Identifier: Apache-2.0

package replication

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
)

// s3ReplicaClient replicates objects to a single destination endpoint using
// unsigned-payload SigV4 PUT for data transfer and DELETE for removal.
type s3ReplicaClient struct {
	dst    Destination
	bucket string
	cred   auth.Credentials
	client *http.Client
}

// NewClient builds a ReplicaClient for the given rule. Bucket falls back to
// the source bucket name when dst.Bucket is empty; the caller passes the
// actual source bucket name.
func NewClient(dst Destination, sourceBucket string) ReplicaClient {
	bucket := dst.Bucket
	if bucket == "" {
		bucket = sourceBucket
	}
	return &s3ReplicaClient{
		dst:    dst,
		bucket: bucket,
		cred:   auth.Credentials{AccessKey: dst.AccessKey, SecretKey: dst.SecretKey},
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// PutObject implements ReplicaClient: PUTs the object to the destination.
// When isReplica is true the REPLICA status header is sent so the destination
// records the object as a replica (preventing re-replication in active-active).
func (c *s3ReplicaClient) PutObject(ctx context.Context, bucket, key string, body io.Reader, size int64, userMeta map[string]string, isReplica bool) error {
	url := fmt.Sprintf("%s/%s/%s", c.dst.Endpoint, c.bucket, key)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, body)
	if err != nil {
		return fmt.Errorf("replication: build PUT %s/%s: %w", c.bucket, key, err)
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")
	for k, v := range userMeta {
		if k == "content-type" {
			req.Header.Set("Content-Type", v)
			continue
		}
		req.Header.Set("x-amz-meta-"+k, v)
	}
	if isReplica {
		req.Header.Set("x-amz-replication-status", string(StatusReplica))
	}
	region := c.dst.Region
	if region == "" {
		region = "us-east-1"
	}
	sign.SignHeader(req, c.cred, region, sign.UnsignedPayload, time.Now().UTC())
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("replication: PUT %s/%s: %w", c.bucket, key, err)
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("replication: PUT %s/%s returned %d", c.bucket, key, resp.StatusCode)
	}
	return nil
}

// DeleteObject implements ReplicaClient: deletes the key/versionID from the destination.
func (c *s3ReplicaClient) DeleteObject(ctx context.Context, bucket, key, versionID string) error {
	url := fmt.Sprintf("%s/%s/%s", c.dst.Endpoint, c.bucket, key)
	if versionID != "" {
		url += "?versionId=" + versionID
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("replication: build DELETE %s/%s: %w", c.bucket, key, err)
	}
	region := c.dst.Region
	if region == "" {
		region = "us-east-1"
	}
	sign.SignHeader(req, c.cred, region, sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("replication: DELETE %s/%s: %w", c.bucket, key, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("replication: DELETE %s/%s returned %d", c.bucket, key, resp.StatusCode)
	}
	return nil
}

// contentLength stringifies n for HTTP headers.
func contentLength(n int64) string { return strconv.FormatInt(n, 10) }

var _ = contentLength // ensure the function is referenced
