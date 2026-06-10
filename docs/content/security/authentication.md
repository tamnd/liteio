---
title: "Authentication"
description: "SigV4 request signing and presigned URL authentication."
weight: 10
---

Every request to liteio must carry a valid SigV4 signature or be denied with
`403 AccessDenied`. liteio implements the same signature algorithm as AWS S3, so
unmodified S3 clients work without any adapter.

## How SigV4 works

The client hashes the request method, URL, headers, and body (or a streaming
trailer), signs the hash with an HMAC-SHA256 key derived from the secret key,
and includes the signature in either the `Authorization` header or the query
string (presigned).

liteio verifies the signature and checks the request timestamp. Requests older
than 15 minutes are rejected with `RequestTimeTooSkewed`.

## Credentials

The root credential is set at server start with `--access-key` and
`--secret-key`. Use it to bootstrap the IAM store — create users with less
privilege and use those for day-to-day operations.

IAM users and service accounts have their own access key pairs managed through
the admin API or the console.

## Presigned URLs

A presigned URL is a time-limited, bearer-token URL that authorizes a single
operation. Generate one with the AWS CLI:

```bash
# Presigned GET (valid 1 hour).
aws s3 presign s3://my-bucket/file.txt \
  --expires-in 3600 \
  --profile liteio
```

```bash
# Download with curl — no AWS credentials needed.
curl "https://liteio.example.com/my-bucket/file.txt?X-Amz-..."
```

Presigned PUT URLs:

```bash
aws s3 presign s3://my-bucket/upload.bin \
  --expires-in 3600 \
  --profile liteio
# Returns a presigned URL. Upload with:
curl -X PUT --upload-file upload.bin "$PRESIGNED_URL"
```

## STS temporary credentials

The STS endpoint issues short-lived access keys bound to an IAM role. The
caller assumes a role (presenting a JWT, LDAP credential, or X.509
certificate), receives a temporary key pair plus a session token, and uses them
to sign requests. The session token is passed as the
`X-Amz-Security-Token` header.

See the [federation page](/security/federation/) for OIDC, LDAP, and
certificate-based STS flows.

## Anonymous access

If no credentials are present on a request, liteio evaluates the request as the
anonymous principal. Bucket policies can grant anonymous read:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Effect": "Allow",
            "Principal": "*",
            "Action": "s3:GetObject",
            "Resource": "arn:aws:s3:::public-bucket/*"
        }
    ]
}
```

Without a permissive bucket policy, anonymous requests are denied.
