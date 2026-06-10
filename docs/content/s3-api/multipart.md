---
title: "Multipart upload"
description: "Upload large objects in parallel parts."
weight: 40
---

Multipart upload lets you split a large object into independently uploaded
parts, then assemble them server-side. The AWS CLI uses multipart automatically
for files over 8 MB with `aws s3 cp`; `aws s3api` lets you control each step.

## When to use multipart

- Objects over 100 MB benefit from parallel part uploads.
- Objects over 5 GB require multipart (the single-part PUT limit is 5 GB).
- Resumable uploads: store the `UploadId` and resume from the last successful
  part after a network failure.

## High-level (AWS CLI)

For most cases, `aws s3 cp` handles multipart transparently:

```bash
aws s3 cp large-file.bin s3://my-bucket/large-file.bin \
  --expected-size 1073741824 \
  --profile liteio
```

## Low-level (step by step)

### 1. Start the upload

```bash
aws s3api create-multipart-upload \
  --bucket my-bucket \
  --key large-file.bin \
  --profile liteio
```

```json
{
    "Bucket": "my-bucket",
    "Key": "large-file.bin",
    "UploadId": "2~abc123..."
}
```

Save the `UploadId` for subsequent calls.

### 2. Upload each part

Parts must be at least 5 MB (except the last part). Upload them in any order:

```bash
# Part 1 (first 100 MB)
aws s3api upload-part \
  --bucket my-bucket \
  --key large-file.bin \
  --upload-id "$UPLOAD_ID" \
  --part-number 1 \
  --body part1.bin \
  --profile liteio
```

```json
{
    "ETag": "\"abc001\""
}
```

Repeat for each part. Record the `PartNumber` and `ETag` from each response.

### 3. Complete the upload

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
{
    "Bucket": "my-bucket",
    "Key": "large-file.bin",
    "ETag": "\"def456-3\""
}
```

The final ETag is the MD5 of the concatenated part ETags followed by a `-N`
suffix (where N is the part count), as the S3 spec defines.

### Abort a failed upload

```bash
aws s3api abort-multipart-upload \
  --bucket my-bucket \
  --key large-file.bin \
  --upload-id "$UPLOAD_ID" \
  --profile liteio
```

### List in-progress uploads

```bash
aws s3api list-multipart-uploads \
  --bucket my-bucket \
  --profile liteio
```

### List uploaded parts

```bash
aws s3api list-parts \
  --bucket my-bucket \
  --key large-file.bin \
  --upload-id "$UPLOAD_ID" \
  --profile liteio
```

## UploadPartCopy

Copy part of an existing object as a multipart part without re-uploading the
data:

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
