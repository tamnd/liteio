---
title: "Buckets"
description: "Create, list, delete, and configure buckets."
weight: 10
---

## Create a bucket

```bash
aws s3api create-bucket \
  --bucket my-bucket \
  --profile liteio
```

```json
{
    "Location": "/my-bucket"
}
```

`CreateBucket` returns `409 BucketAlreadyOwnedByYou` if you already own the
bucket under any region — liteio does not reproduce the legacy `us-east-1`
exception that returns 200 and resets the ACL.

## List buckets

```bash
aws s3api list-buckets --profile liteio
```

```json
{
    "Buckets": [
        {
            "Name": "my-bucket",
            "CreationDate": "2026-06-10T12:00:00.000Z"
        }
    ],
    "Owner": {
        "DisplayName": "admin",
        "ID": "admin"
    }
}
```

## Delete a bucket

A bucket must be empty before it can be deleted.

```bash
aws s3api delete-bucket --bucket my-bucket --profile liteio
```

## Get bucket location

```bash
aws s3api get-bucket-location --bucket my-bucket --profile liteio
```

```json
{
    "LocationConstraint": null
}
```

liteio always reports `null` (equivalent to `us-east-1`) regardless of any
constraint passed at creation time.

## Versioning

Enable versioning to preserve every version of an object instead of overwriting
it on `PUT`.

```bash
aws s3api put-bucket-versioning \
  --bucket my-bucket \
  --versioning-configuration Status=Enabled \
  --profile liteio
```

```bash
aws s3api get-bucket-versioning --bucket my-bucket --profile liteio
```

```json
{
    "Status": "Enabled"
}
```

Suspended versioning stops creating new versions but keeps existing ones. Delete
an object with versioning enabled to create a delete marker rather than remove
the data.

## Lifecycle

Lifecycle rules automate expiration and transition. Store rules as a JSON file:

```json
{
    "Rules": [
        {
            "ID": "expire-old-logs",
            "Status": "Enabled",
            "Filter": { "Prefix": "logs/" },
            "Expiration": { "Days": 30 }
        }
    ]
}
```

```bash
aws s3api put-bucket-lifecycle-configuration \
  --bucket my-bucket \
  --lifecycle-configuration file://lifecycle.json \
  --profile liteio

aws s3api get-bucket-lifecycle-configuration \
  --bucket my-bucket \
  --profile liteio
```

## Tagging

```bash
aws s3api put-bucket-tagging \
  --bucket my-bucket \
  --tagging 'TagSet=[{Key=env,Value=prod},{Key=team,Value=infra}]' \
  --profile liteio

aws s3api get-bucket-tagging --bucket my-bucket --profile liteio
aws s3api delete-bucket-tagging --bucket my-bucket --profile liteio
```

## Bucket policy

Bucket policies use the IAM JSON syntax. This policy allows any authenticated
user to read objects:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": "*",
            "Action": "s3:GetObject",
            "Resource": "arn:aws:s3:::my-bucket/*"
        }
    ]
}
```

```bash
aws s3api put-bucket-policy \
  --bucket my-bucket \
  --policy file://policy.json \
  --profile liteio

aws s3api get-bucket-policy --bucket my-bucket --profile liteio
aws s3api delete-bucket-policy --bucket my-bucket --profile liteio
```
