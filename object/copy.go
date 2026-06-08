// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"fmt"
	"io"
)

// CopyObject implements ObjectLayer: it materializes a source object (at the
// version named by srcInfo) and writes it to the destination key with the
// metadata supplied in opts.
//
// The copy is read-then-write: the source is read through its owning set and the
// bytes are written through the destination's owning set, so a copy that crosses
// pools or sets is handled by the same routing PUT/GET use. A same-set shard-level
// copy that skips re-encoding is a documented later optimization (spec doc 06);
// correctness comes first. Destination metadata is whatever the front door places
// in opts — the COPY-vs-REPLACE directive is resolved there.
func (sp *ServerPools) CopyObject(ctx context.Context, srcBucket, srcObject, dstBucket, dstObject string, srcInfo ObjectInfo, opts ObjectOptions) (ObjectInfo, error) {
	data, err := sp.readWholeObject(ctx, srcBucket, srcObject, srcInfo.VersionID, opts.SrcSSECKey)
	if err != nil {
		return ObjectInfo{}, err
	}
	putOpts := opts
	putOpts.SrcSSECKey = nil // src key does not apply to the destination
	return sp.PutObject(ctx, dstBucket, dstObject, NewPutReader(bytes.NewReader(data), int64(len(data))), putOpts)
}

// CopyObjectPart implements ObjectLayer: it reads a source object (optionally a
// byte range of it) and writes those bytes as part partID of an existing
// multipart upload on the destination key.
func (sp *ServerPools) CopyObjectPart(ctx context.Context, srcBucket, srcObject, dstBucket, dstObject, uploadID string, partID int, srcInfo ObjectInfo, rng *HTTPRangeSpec, opts ObjectOptions) (PartInfo, error) {
	gr, err := sp.GetObject(ctx, srcBucket, srcObject, ObjectOptions{VersionID: srcInfo.VersionID, Range: rng, SSECKey: opts.SrcSSECKey})
	if err != nil {
		return PartInfo{}, err
	}
	defer func() { _ = gr.Close() }()
	data, err := io.ReadAll(gr)
	if err != nil {
		return PartInfo{}, fmt.Errorf("object: read copy source: %w", err)
	}
	return sp.PutObjectPart(ctx, dstBucket, dstObject, uploadID, partID, NewPutReader(bytes.NewReader(data), int64(len(data))), ObjectOptions{})
}

// readWholeObject reads an entire object version into memory. key, when non-nil,
// is the SSE-C customer key for decryption.
func (sp *ServerPools) readWholeObject(ctx context.Context, bucket, object, versionID string, key *[32]byte) ([]byte, error) {
	gr, err := sp.GetObject(ctx, bucket, object, ObjectOptions{VersionID: versionID, SSECKey: key})
	if err != nil {
		return nil, err
	}
	defer func() { _ = gr.Close() }()
	data, err := io.ReadAll(gr)
	if err != nil {
		return nil, fmt.Errorf("object: read copy source: %w", err)
	}
	return data, nil
}
