---
title: "Authentication"
description: "SigV4 signing, presigned URLs, and temporary credentials."
weight: 10
---

Every request is signed with SigV4 or it is denied. liteio runs the same
signature algorithm as AWS S3, so your existing clients sign correctly with no
changes.

## How a request is trusted

The client hashes the method, path, headers, and body, signs that hash with a
key derived from the secret key, and attaches the signature in the
`Authorization` header or the query string. liteio recomputes the signature and
checks the timestamp. A request more than 15 minutes old is rejected with
`RequestTimeTooSkewed`, which is what stops a captured request from being
replayed later.

## Credentials

`--access-key` and `--secret-key` set the root credential at startup. Use it to
bootstrap, then create scoped-down IAM users and stop using root for day-to-day
traffic. IAM users and service accounts each carry their own key pair, managed
through the admin API or the console.

## Presigned URLs

A presigned URL authorizes one operation for a fixed window, with no credentials
on the far end. Generate a download link good for an hour:

```bash
aws s3 presign s3://my-bucket/file.txt --expires-in 3600 --profile liteio
```

Anyone with the URL can fetch it:

```bash
curl "https://liteio.example.com/my-bucket/file.txt?X-Amz-..."
```

Presigned PUT works the same way, which is the usual pattern for browser
uploads that never see your secret key:

```bash
aws s3 presign s3://my-bucket/upload.bin --expires-in 3600 --profile liteio
curl -X PUT --upload-file upload.bin "$PRESIGNED_URL"
```

## Temporary credentials

STS hands out short-lived keys tied to a role. A caller proves who they are with
a JWT, an LDAP login, or a client certificate, and gets back a temporary access
key, secret key, and session token. The session token travels in the
`X-Amz-Security-Token` header and expires on its own, so a leak is bounded in
time.

The full OIDC, LDAP, and certificate flows are on the
[federation page](/security/federation/).

## Anonymous requests

A request with no credentials is evaluated as the anonymous principal. It is
denied unless a bucket policy opens the door. This grants public read on one
bucket:

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

Without a policy like this, anonymous access gets `403 AccessDenied`. Nothing is
public by default.
