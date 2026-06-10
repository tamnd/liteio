---
title: "Compatibility matrix"
description: "Every S3 operation and its implementation status."
weight: 10
---

This table is the living source of truth for what liteio supports. It is
updated as each milestone lands. Use it to evaluate whether liteio fits your
workload before migrating.

## S3 operations

| Operation | Status | Notes |
|---|---|---|
| `PutObject` | implemented | Single-part, up to 5 GB |
| `GetObject` | implemented | Range, conditional, presigned |
| `HeadObject` | implemented | |
| `DeleteObject` | implemented | |
| `DeleteObjects` (batch) | implemented | Up to 1,000 keys per request |
| `CopyObject` | implemented | Within and across buckets |
| `CreateMultipartUpload` | implemented | |
| `UploadPart` | implemented | |
| `UploadPartCopy` | implemented | |
| `CompleteMultipartUpload` | implemented | |
| `AbortMultipartUpload` | implemented | |
| `ListMultipartUploads` | implemented | |
| `ListParts` | implemented | |
| `ListObjectsV2` | implemented | Prefix, delimiter, pagination |
| `ListObjects` (v1) | implemented | |
| `ListObjectVersions` | implemented | |
| `CreateBucket` | implemented | |
| `DeleteBucket` | implemented | |
| `HeadBucket` | implemented | |
| `GetBucketLocation` | implemented | Always returns `null` (us-east-1) |
| `ListBuckets` | implemented | |
| `PutBucketVersioning` | implemented | |
| `GetBucketVersioning` | implemented | |
| `PutBucketPolicy` | implemented | Full IAM syntax |
| `GetBucketPolicy` | implemented | |
| `DeleteBucketPolicy` | implemented | |
| `PutBucketTagging` | implemented | |
| `GetBucketTagging` | implemented | |
| `DeleteBucketTagging` | implemented | |
| `PutObjectTagging` | implemented | |
| `GetObjectTagging` | implemented | |
| `DeleteObjectTagging` | implemented | |
| `PutBucketLifecycleConfiguration` | implemented | XML stored; scanner pending |
| `GetBucketLifecycleConfiguration` | implemented | |
| `DeleteBucketLifecycleConfiguration` | implemented | |
| SSE-C (`x-amz-server-side-encryption-customer-*`) | implemented | AES-256-CTR |
| `AssumeRole` (STS) | implemented | |
| `AssumeRoleWithWebIdentity` (OIDC) | implemented | |
| `AssumeRoleWithLDAPIdentity` | implemented | |
| `AssumeRoleWithCertificate` | implemented | |
| Lifecycle scanner execution | planned (M6) | |
| Bucket replication | planned (M7) | |
| Object tiering | planned (M7) | |
| Rebalance / decommission | planned (M8) | |
| `SelectObjectContent` | not implemented | Returns `NotImplemented` |
| S3 Object Lambda | not implemented | Returns `NotImplemented` |
| Access Points | not implemented | Returns `NotImplemented` |
| S3 Inventory / Analytics | not implemented | Returns `NotImplemented` |
| S3 Batch Operations | not implemented | Returns `NotImplemented` |
| Transfer Acceleration | not implemented | Returns `NotImplemented` |
| Requester Pays | not implemented | Returns `NotImplemented` |

## Authentication

| Feature | Status |
|---|---|
| SigV4 (header) | implemented |
| SigV4 presigned URLs | implemented |
| SigV4 streaming (`aws-chunked`) | implemented |
| STS temporary credentials | implemented |
| OIDC web identity federation | implemented |
| LDAP federation | implemented |
| Certificate federation | implemented |

## IAM

| Feature | Status |
|---|---|
| Users and access keys | implemented |
| Groups | implemented |
| Service accounts | implemented |
| PBAC policy evaluation (AWS IAM syntax) | implemented |
| Bucket policies | implemented |
| Anonymous (public) access | implemented |
| STS session intersection | implemented |
| IAM condition keys | implemented |

## Deliberate divergences

These behaviors differ from AWS S3 by design, not by accident.

**`CreateBucket` idempotency**: liteio returns `409 BucketAlreadyOwnedByYou`
when you create a bucket you already own in any region, including `us-east-1`.
AWS S3 returns `200 OK` for `us-east-1` as a historical exception; liteio does
not reproduce this because it creates a surprising success for what is logically
a conflict.

**ACL normalization**: Arbitrary grantee-based ACL grants (`CanonicalUser`,
email grants) are normalized to the nearest canned ACL
(`private` / `public-read` / `public-read-write` / `authenticated-read`).
Finely-grained ACL grants have no useful meaning outside a full AWS account
model.

**Unsupported operations**: Deferred operations return a proper
`501 NotImplemented` S3 error with `<Code>NotImplemented</Code>` rather than
failing with a misleading 400 or 403.
