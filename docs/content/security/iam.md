---
title: "IAM"
description: "Users, groups, service accounts, and policies."
weight: 20
---

liteio implements the AWS IAM PBAC model: every action is evaluated against the
IAM policy attached to the identity making the request. Deny always wins. A
request with no matching allow is denied.

## Users

Create a user with the admin API (authenticated with your root credential):

```bash
curl -X POST http://localhost:9001/minio/v1/add-user \
  -u admin:changeme \
  -d '{"accessKey":"alice","secretKey":"alicepw","policy":""}'
```

Or use the `mc` admin plugin:

```bash
mc admin user add local alice alicepw
```

List users:

```bash
mc admin user list local
```

## Policies

Policies are JSON documents using the AWS IAM syntax. Save this as
`read-only.json`:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Action": [
                "s3:GetObject",
                "s3:GetObjectVersion",
                "s3:ListBucket",
                "s3:ListBucketVersions"
            ],
            "Resource": [
                "arn:aws:s3:::*",
                "arn:aws:s3:::*/*"
            ]
        }
    ]
}
```

Add the policy to liteio:

```bash
mc admin policy create local read-only read-only.json
```

Attach it to a user:

```bash
mc admin policy attach local read-only --user alice
```

### Canned policies

liteio ships four built-in policies matching the MinIO canned set:

| Name | Description |
|---|---|
| `readwrite` | Full S3 access on all buckets |
| `readonly` | Read and list access on all buckets |
| `writeonly` | Write and delete access on all buckets |
| `diagnostics` | Read cluster health and metrics |

## Groups

Groups let you attach a policy to multiple users at once.

```bash
mc admin group add local devs alice bob charlie
mc admin policy attach local read-only --group devs
```

## Service accounts

Service accounts are long-lived access keys scoped to a parent user's policy
plus an optional further restriction:

```bash
mc admin user svcacct add local alice \
  --name "ci-pipeline" \
  --policy '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::builds/*"}]}'
```

The service account receives an access key and secret key. Its effective policy
is the intersection of the parent user's policy and the service-account policy.

## Condition keys

Policies support condition keys to constrain access by IP, date, object prefix,
and other attributes:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Deny",
            "Action": "*",
            "Resource": "*",
            "Condition": {
                "NotIpAddress": {
                    "aws:SourceIp": ["10.0.0.0/8", "192.168.0.0/16"]
                }
            }
        }
    ]
}
```

Supported condition operators: string (StringEquals, StringLike, ...), numeric,
date, boolean, IP (IpAddress, NotIpAddress), and their `IfExists` and `Not`
variants.
