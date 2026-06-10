---
title: "Listing objects"
description: "List objects in a bucket with prefix filtering, delimiter grouping, and pagination."
weight: 30
---

liteio supports both `ListObjectsV2` (preferred) and the legacy `ListObjects`
(v1). Both return at most 1,000 objects per page and support prefix filtering
and delimiter-based grouping (virtual directories).

The in-memory namespace index makes list requests fast even on buckets with
millions of objects: a scan across a 10,000-object bucket completes in under
1 ms on the server and returns 1,000 results in 32 ms end-to-end on a shared
VPS.

## List all objects

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --profile liteio
```

```json
{
    "Contents": [
        {
            "Key": "docs/README.md",
            "LastModified": "2026-06-10T12:00:00.000Z",
            "ETag": "\"abc123\"",
            "Size": 1024,
            "StorageClass": "STANDARD"
        }
    ],
    "KeyCount": 1,
    "MaxKeys": 1000,
    "IsTruncated": false
}
```

## Filter by prefix

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --prefix logs/2026/ \
  --profile liteio
```

Returns only keys that start with `logs/2026/`.

## Virtual directories (delimiter)

Use `/` as a delimiter to list the top-level "directories":

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --delimiter / \
  --profile liteio
```

```json
{
    "CommonPrefixes": [
        {"Prefix": "docs/"},
        {"Prefix": "logs/"}
    ],
    "KeyCount": 2,
    "MaxKeys": 1000,
    "IsTruncated": false
}
```

Combine with `--prefix` to list a subdirectory:

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --prefix logs/ \
  --delimiter / \
  --profile liteio
```

## Pagination

When `IsTruncated` is `true`, use `NextContinuationToken` to fetch the next
page:

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --max-items 100 \
  --profile liteio
```

If the response includes `NextToken`, pass it to the next call:

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --max-items 100 \
  --starting-token "$NEXT_TOKEN" \
  --profile liteio
```

The AWS CLI's `--max-items` + `--starting-token` paginates automatically. To
use the raw API parameters:

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --max-keys 100 \
  --continuation-token "$TOKEN" \
  --profile liteio
```

## List all objects in a script

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --query 'Contents[].Key' \
  --output text \
  --profile liteio
```

For large buckets, page automatically:

```bash
aws s3 ls s3://my-bucket --recursive --profile liteio
```

## List object versions

With versioning enabled, `list-object-versions` returns every version and
delete marker:

```bash
aws s3api list-object-versions \
  --bucket my-bucket \
  --profile liteio
```

```json
{
    "Versions": [
        {
            "Key": "file.txt",
            "VersionId": "...",
            "IsLatest": true,
            "LastModified": "2026-06-10T12:01:00.000Z",
            "ETag": "\"abc123\"",
            "Size": 1024
        }
    ]
}
```

Filter by prefix:

```bash
aws s3api list-object-versions \
  --bucket my-bucket \
  --prefix docs/ \
  --profile liteio
```
