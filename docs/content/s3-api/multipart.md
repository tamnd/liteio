---
title: "Multipart upload"
description: "Upload large objects as parallel parts."
weight: 40
---

Multipart upload splits a large object into parts you upload independently, then
liteio stitches them together server-side. The AWS CLI switches to multipart on
its own for files over 8 MB; `aws s3api` gives you each step when you need
control.

Reach for it when:

- An object is over 100 MB and you want parallel part uploads.
- An object is over 5 GB, the single-part PUT ceiling, so multipart is required.
- You need resumable uploads. Keep the `UploadId` and re-send only the parts
  that failed.

## The easy way

For most uploads, `aws s3 cp` handles multipart transparently:

```bash
aws s3 cp large-file.bin s3://my-bucket/large-file.bin --profile liteio
```

## Step by step

### 1. Start

```bash
aws s3api create-multipart-upload \
  --bucket my-bucket \
  --key large-file.bin \
  --profile liteio
```

```json
{ "Bucket": "my-bucket", "Key": "large-file.bin", "UploadId": "2~abc123..." }
```

Hold onto the `UploadId`. Every later call needs it.

### 2. Upload the parts

Each part is at least 5 MB, except the last. Upload them in any order, in
parallel if you like, and record the `PartNumber` and `ETag` from each reply:

```bash
aws s3api upload-part \
  --bucket my-bucket \
  --key large-file.bin \
  --upload-id "$UPLOAD_ID" \
  --part-number 1 \
  --body part1.bin \
  --profile liteio
```

```json
{ "ETag": "\"abc001\"" }
```

### 3. Complete

Hand back the parts in ascending order:

```bash
aws s3api complete-multipart-upload \
  --bucket my-bucket \
  --key large-file.bin \
  --upload-id "$UPLOAD_ID" \
  --multipart-upload '{
    "Parts": [
      {"PartNumber": 1, "ETag": "\"abc001\""},
      {"PartNumber": 2, "ETag": "\"abc002\""},
      {"PartNumber": 3, "ETag": "\"abc003\""}
    ]
  }' \
  --profile liteio
```

```json
{ "Bucket": "my-bucket", "Key": "large-file.bin", "ETag": "\"def456-3\"" }
```

The final ETag is the MD5 of the concatenated part ETags with a `-N` suffix for
the part count, the same scheme AWS uses. It is not the MD5 of the whole object,
so do not compare it against a local `md5sum`.

### Abort

Give up on an upload and free its parts:

```bash
aws s3api abort-multipart-upload \
  --bucket my-bucket \
  --key large-file.bin \
  --upload-id "$UPLOAD_ID" \
  --profile liteio
```

## Inspect uploads in flight

```bash
aws s3api list-multipart-uploads --bucket my-bucket --profile liteio

aws s3api list-parts \
  --bucket my-bucket \
  --key large-file.bin \
  --upload-id "$UPLOAD_ID" \
  --profile liteio
```

## Copy a range into a part

Assemble a new object from a slice of an existing one without pulling the bytes
through your client:

```bash
aws s3api upload-part-copy \
  --bucket dest-bucket \
  --key assembled.bin \
  --upload-id "$UPLOAD_ID" \
  --part-number 1 \
  --copy-source source-bucket/large-file.bin \
  --copy-source-range 'bytes=0-104857599' \
  --profile liteio
```
