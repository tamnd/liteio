---
title: "Listing objects"
description: "List with prefix filters, delimiter grouping, and pagination."
weight: 30
---

liteio serves both `ListObjectsV2` (use this) and the legacy `ListObjects` v1.
Both page at 1,000 objects, filter by prefix, and group by delimiter into
virtual directories.

Listing is fast even on big buckets. An in-memory namespace index answers the
query without touching disk, so a scan over 10,000 objects finishes in under a
millisecond on the server, and a full 1,000-object page comes back in 32 ms over
the wire on a modest VPS. Most of that 32 ms is network and OS scheduling, not
liteio.

## Everything

```bash
aws s3api list-objects-v2 --bucket my-bucket --profile liteio
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

## By prefix

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --prefix logs/2026/ \
  --profile liteio
```

Returns only keys under `logs/2026/`.

## As directories

A `/` delimiter collapses everything below each first slash into a common
prefix, so you get the top-level folders instead of every key:

```bash
aws s3api list-objects-v2 --bucket my-bucket --delimiter / --profile liteio
```

```json
{
    "CommonPrefixes": [
        { "Prefix": "docs/" },
        { "Prefix": "logs/" }
    ],
    "KeyCount": 2,
    "MaxKeys": 1000,
    "IsTruncated": false
}
```

Add a prefix to descend one level at a time, the way a file browser walks a
tree:

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --prefix logs/ \
  --delimiter / \
  --profile liteio
```

## Paging

When `IsTruncated` is `true`, the response carries a `NextContinuationToken`.
Feed it back to get the next page. The AWS CLI does this for you with
`--max-items` and `--starting-token`:

```bash
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --max-items 100 \
  --profile liteio
# If the output shows NextToken, pass it back:
aws s3api list-objects-v2 \
  --bucket my-bucket \
  --max-items 100 \
  --starting-token "$NEXT_TOKEN" \
  --profile liteio
```

To drive the raw API yourself, use `--max-keys` and `--continuation-token`. Or
just let the high-level command page for you:

```bash
aws s3 ls s3://my-bucket --recursive --profile liteio
```

## Versions

With versioning on, `list-object-versions` returns every version and delete
marker, newest first:

```bash
aws s3api list-object-versions --bucket my-bucket --profile liteio
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

It takes the same `--prefix` filter as the object listings.
