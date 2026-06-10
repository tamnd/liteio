---
title: "IAM"
description: "Users, groups, service accounts, and policies."
weight: 20
---

liteio evaluates every action against the IAM policy attached to the identity
making the request. The rules are the AWS ones: deny always beats allow, and
anything not explicitly allowed is denied. There is no implicit trust.

## Users

Create a user with the admin API, authenticated as root:

```bash
curl -X POST http://localhost:9001/minio/v1/add-user \
  -u admin:changeme \
  -d '{"accessKey":"alice","secretKey":"alicepw","policy":""}'
```

Or use the MinIO admin client, which talks to the same API:

```bash
mc admin user add  local alice alicepw
mc admin user list local
```

## Policies

A policy is an IAM JSON document. This one is read-only across all buckets. Save
it as `read-only.json`:

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

Load it and attach it to a user:

```bash
mc admin policy create local read-only read-only.json
mc admin policy attach local read-only --user alice
```

### Built-in policies

Four canned policies ship ready to attach, matching the names MinIO users
expect:

| Name | Grants |
|---|---|
| `readwrite` | Full S3 access on every bucket |
| `readonly` | Read and list on every bucket |
| `writeonly` | Write and delete on every bucket |
| `diagnostics` | Cluster health and metrics |

## Groups

A group attaches one policy to many users at once:

```bash
mc admin group  add    local devs alice bob charlie
mc admin policy attach local read-only --group devs
```

## Service accounts

A service account is a long-lived key pair that inherits its parent user's
policy, optionally narrowed further. The effective permission is the
intersection of the two, so a service account can never out-reach the user it
belongs to:

```bash
mc admin user svcacct add local alice \
  --name "ci-pipeline" \
  --policy '{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::builds/*"}]}'
```

This one can write to `builds/` and nothing else, even if alice can do far more.

## Conditions

Condition keys constrain a statement by source IP, date, object prefix, and
more. This denies everything from outside two networks, regardless of any allow
elsewhere, because deny wins:

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

The supported operators are the AWS set: string, numeric, date, boolean, and IP,
each with its `IfExists` and `Not` variants.
