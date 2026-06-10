---
title: "Buckets"
description: "Create, list, delete, and configure buckets."
weight: 10
---

## Create

```bash
aws s3api create-bucket --bucket my-bucket --profile liteio
```

```json
{
    "Location": "/my-bucket"
}
```

If you already own the bucket, this returns `409 BucketAlreadyOwnedByYou` in
every region. liteio does not reproduce the legacy `us-east-1` quirk where the
same call returns `200` and resets the ACL. See the
[compatibility notes](/reference/compatibility/) for why.

## List

```bash
aws s3api list-buckets --profile liteio
```

```json
{
    "Buckets": [
        { "Name": "my-bucket", "CreationDate": "2026-06-10T12:00:00.000Z" }
    ],
    "Owner": { "DisplayName": "admin", "ID": "admin" }
}
```

## Delete

A bucket has to be empty first.

```bash
aws s3api delete-bucket --bucket my-bucket --profile liteio
```

## Location

```bash
aws s3api get-bucket-location --bucket my-bucket --profile liteio
```

```json
{ "LocationConstraint": null }
```

liteio reports `null`, the value AWS uses for `us-east-1`, no matter what
constraint you passed at creation. There is one namespace, not per-region
endpoints.

## Versioning

Turn on versioning to keep every version of a key instead of overwriting it.

```bash
aws s3api put-bucket-versioning \
  --bucket my-bucket \
  --versioning-configuration Status=Enabled \
  --profile liteio

aws s3api get-bucket-versioning --bucket my-bucket --profile liteio
```

```json
{ "Status": "Enabled" }
```

Suspending versioning stops new versions but keeps the ones you have. With
versioning on, deleting a key writes a delete marker rather than removing the
data, so a delete is reversible.

## Lifecycle

Lifecycle rules expire or transition objects on a schedule. Put the rules in a
JSON file:

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

aws s3api get-bucket-lifecycle-configuration --bucket my-bucket --profile liteio
```

> liteio stores and serves lifecycle configuration today. The scanner that
> enforces expiration on a schedule is on the roadmap, so rules are recorded but
> not yet acted on automatically. Track it in the
> [compatibility matrix](/reference/compatibility/).

## Tagging

```bash
aws s3api put-bucket-tagging \
  --bucket my-bucket \
  --tagging 'TagSet=[{Key=env,Value=prod},{Key=team,Value=infra}]' \
  --profile liteio

aws s3api get-bucket-tagging    --bucket my-bucket --profile liteio
aws s3api delete-bucket-tagging --bucket my-bucket --profile liteio
```

## Bucket policy

Bucket policies use IAM JSON. This one lets anyone read objects in the bucket:

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

aws s3api get-bucket-policy    --bucket my-bucket --profile liteio
aws s3api delete-bucket-policy --bucket my-bucket --profile liteio
```

The policy language and evaluation rules are covered in [IAM](/security/iam/).
