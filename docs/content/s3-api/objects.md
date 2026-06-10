---
title: "Objects"
description: "Upload, download, stat, and delete objects."
weight: 20
---

## Upload (PUT)

```bash
aws s3api put-object \
  --bucket my-bucket \
  --key path/to/file.txt \
  --body file.txt \
  --profile liteio
```

```json
{
    "ETag": "\"d41d8cd98f00b204e9800998ecf8427e\""
}
```

The `ETag` is the MD5 hex digest of the object body, quoted as required by the
S3 spec.

### Upload with metadata

```bash
aws s3api put-object \
  --bucket my-bucket \
  --key photo.jpg \
  --body photo.jpg \
  --content-type image/jpeg \
  --metadata '{"author":"alice","project":"website"}' \
  --profile liteio
```

User-defined metadata is stored as `x-amz-meta-*` headers and returned on GET
and HEAD.

### Upload with tagging

```bash
aws s3api put-object \
  --bucket my-bucket \
  --key photo.jpg \
  --body photo.jpg \
  --tagging 'env=prod&project=website' \
  --profile liteio
```

### Presigned upload

Generate a presigned PUT URL valid for one hour:

```bash
aws s3 presign s3://my-bucket/upload.bin \
  --expires-in 3600 \
  --profile liteio
```

Use the URL with any HTTP client:

```bash
curl -X PUT --upload-file upload.bin "$PRESIGNED_URL"
```

## Download (GET)

```bash
aws s3api get-object \
  --bucket my-bucket \
  --key path/to/file.txt \
  output.txt \
  --profile liteio
```

```json
{
    "ContentType": "application/octet-stream",
    "ContentLength": 1234,
    "ETag": "\"d41d8cd98f00b204e9800998ecf8427e\""
}
```

### Range GET

Download a byte range (useful for large files):

```bash
aws s3api get-object \
  --bucket my-bucket \
  --key large-file.bin \
  --range 'bytes=0-1048575' \
  output-part0.bin \
  --profile liteio
```

liteio returns `206 Partial Content` with the `Content-Range` header.

### Conditional GET

liteio supports all four conditional headers:

```bash
# Only download if the ETag matches (If-Match).
aws s3api get-object \
  --bucket my-bucket \
  --key file.txt \
  output.txt \
  --if-match '"d41d8cd98f00b204e9800998ecf8427e"' \
  --profile liteio

# Only download if the object has changed (If-None-Match).
aws s3api get-object \
  --bucket my-bucket \
  --key file.txt \
  output.txt \
  --if-none-match '"d41d8cd98f00b204e9800998ecf8427e"' \
  --profile liteio
```

`If-Modified-Since` and `If-Unmodified-Since` are also supported.

### Presigned download

```bash
aws s3 presign s3://my-bucket/photo.jpg \
  --expires-in 3600 \
  --profile liteio
```

## HEAD (metadata only)

```bash
aws s3api head-object \
  --bucket my-bucket \
  --key file.txt \
  --profile liteio
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
aws s3api delete-object \
  --bucket my-bucket \
  --key file.txt \
  --profile liteio
```

With versioning enabled, delete creates a delete marker. Pass `--version-id` to
permanently remove a specific version.

## Batch delete

Delete up to 1,000 objects in one request:

```bash
aws s3api delete-objects \
  --bucket my-bucket \
  --delete '{"Objects":[{"Key":"a.txt"},{"Key":"b.txt"},{"Key":"c.txt"}]}' \
  --profile liteio
```

```json
{
    "Deleted": [
        {"Key": "a.txt"},
        {"Key": "b.txt"},
        {"Key": "c.txt"}
    ]
}
```

## Copy

Copy an object within a bucket or between buckets:

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

## Object tagging

```bash
aws s3api put-object-tagging \
  --bucket my-bucket \
  --key file.txt \
  --tagging 'TagSet=[{Key=env,Value=prod}]' \
  --profile liteio

aws s3api get-object-tagging --bucket my-bucket --key file.txt --profile liteio
aws s3api delete-object-tagging --bucket my-bucket --key file.txt --profile liteio
```
