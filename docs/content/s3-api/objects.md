---
title: "Objects"
description: "Upload, download, stat, copy, and delete objects."
weight: 20
---

## Upload

```bash
aws s3api put-object \
  --bucket my-bucket \
  --key path/to/file.txt \
  --body file.txt \
  --profile liteio
```

```json
{ "ETag": "\"d41d8cd98f00b204e9800998ecf8427e\"" }
```

The `ETag` is the MD5 of the body, quoted, exactly as the S3 spec wants it.

### With metadata

User metadata rides along as `x-amz-meta-*` headers and comes back on GET and
HEAD:

```bash
aws s3api put-object \
  --bucket my-bucket \
  --key photo.jpg \
  --body photo.jpg \
  --content-type image/jpeg \
  --metadata '{"author":"alice","project":"website"}' \
  --profile liteio
```

### With tags

```bash
aws s3api put-object \
  --bucket my-bucket \
  --key photo.jpg \
  --body photo.jpg \
  --tagging 'env=prod&project=website' \
  --profile liteio
```

### Presigned upload

Hand a time-limited PUT URL to a client that has no credentials:

```bash
aws s3 presign s3://my-bucket/upload.bin --expires-in 3600 --profile liteio
# Then, from anywhere:
curl -X PUT --upload-file upload.bin "$PRESIGNED_URL"
```

## Download

```bash
aws s3api get-object \
  --bucket my-bucket \
  --key path/to/file.txt \
  output.txt \
  --profile liteio
```

### A byte range

Useful for resuming a transfer or reading a slice of a large file. liteio
answers with `206 Partial Content` and a `Content-Range` header:

```bash
aws s3api get-object \
  --bucket my-bucket \
  --key large-file.bin \
  --range 'bytes=0-1048575' \
  part0.bin \
  --profile liteio
```

### Conditionally

All four conditional headers work. Fetch only if the object changed:

```bash
aws s3api get-object \
  --bucket my-bucket \
  --key file.txt \
  output.txt \
  --if-none-match '"d41d8cd98f00b204e9800998ecf8427e"' \
  --profile liteio
```

A matching `If-None-Match` returns `304 Not Modified`. `If-Match`,
`If-Modified-Since`, and `If-Unmodified-Since` behave the same way they do on
AWS.

## Stat (HEAD)

Metadata without the body:

```bash
aws s3api head-object --bucket my-bucket --key file.txt --profile liteio
```

```json
{
    "ContentType": "application/octet-stream",
    "ContentLength": 1234,
    "ETag": "\"d41d8cd98f00b204e9800998ecf8427e\"",
    "LastModified": "2026-06-10T12:00:00.000Z"
}
```

## Delete

```bash
aws s3api delete-object --bucket my-bucket --key file.txt --profile liteio
```

With versioning on, this writes a delete marker. Pass `--version-id` to remove a
specific version for good.

## Delete in bulk

Up to 1,000 keys in one round trip:

```bash
aws s3api delete-objects \
  --bucket my-bucket \
  --delete '{"Objects":[{"Key":"a.txt"},{"Key":"b.txt"},{"Key":"c.txt"}]}' \
  --profile liteio
```

```json
{
    "Deleted": [
        { "Key": "a.txt" },
        { "Key": "b.txt" },
        { "Key": "c.txt" }
    ]
}
```

## Copy

Server-side, within a bucket or across buckets, with no round trip through the
client:

```bash
aws s3api copy-object \
  --bucket dest-bucket \
  --key new/path/file.txt \
  --copy-source my-bucket/path/to/file.txt \
  --profile liteio
```

```json
{
    "CopyObjectResult": {
        "ETag": "\"d41d8cd98f00b204e9800998ecf8427e\"",
        "LastModified": "2026-06-10T12:01:00.000Z"
    }
}
```

## Object tags

```bash
aws s3api put-object-tagging \
  --bucket my-bucket \
  --key file.txt \
  --tagging 'TagSet=[{Key=env,Value=prod}]' \
  --profile liteio

aws s3api get-object-tagging    --bucket my-bucket --key file.txt --profile liteio
aws s3api delete-object-tagging --bucket my-bucket --key file.txt --profile liteio
```
