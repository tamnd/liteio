---
title: "Compatibility matrix"
description: "Every S3 operation and exactly how far it is supported."
weight: 10
---

This is the honest list of what liteio does today, kept current as milestones
land. Check it before you migrate a workload that leans on a particular feature.

## S3 operations

| Operation | Status | Notes |
|---|---|---|
| `PutObject` | done | Single-part, up to 5 GB |
| `GetObject` | done | Range, conditional, presigned |
| `HeadObject` | done | |
| `DeleteObject` | done | |
| `DeleteObjects` (batch) | done | Up to 1,000 keys per call |
| `CopyObject` | done | Within and across buckets |
| `CreateMultipartUpload` | done | |
| `UploadPart` | done | |
| `UploadPartCopy` | done | |
| `CompleteMultipartUpload` | done | |
| `AbortMultipartUpload` | done | |
| `ListMultipartUploads` | done | |
| `ListParts` | done | |
| `ListObjectsV2` | done | Prefix, delimiter, pagination |
| `ListObjects` (v1) | done | |
| `ListObjectVersions` | done | |
| `CreateBucket` | done | |
| `DeleteBucket` | done | |
| `HeadBucket` | done | |
| `GetBucketLocation` | done | Always `null` (us-east-1) |
| `ListBuckets` | done | |
| `PutBucketVersioning` | done | |
| `GetBucketVersioning` | done | |
| `PutBucketPolicy` | done | Full IAM syntax |
| `GetBucketPolicy` | done | |
| `DeleteBucketPolicy` | done | |
| `PutBucketTagging` | done | |
| `GetBucketTagging` | done | |
| `DeleteBucketTagging` | done | |
| `PutObjectTagging` | done | |
| `GetObjectTagging` | done | |
| `DeleteObjectTagging` | done | |
| `PutBucketLifecycleConfiguration` | done | Stored and served; scanner pending |
| `GetBucketLifecycleConfiguration` | done | |
| `DeleteBucketLifecycleConfiguration` | done | |
| SSE-C | done | AES-256-CTR, customer key |
| `AssumeRole` (STS) | done | |
| `AssumeRoleWithWebIdentity` (OIDC) | done | |
| `AssumeRoleWithLDAPIdentity` | done | |
| `AssumeRoleWithCertificate` | done | |
| Lifecycle scanner execution | roadmap | M6 |
| Bucket replication | roadmap | M7 |
| Object tiering | roadmap | M7 |
| Rebalance and decommission | roadmap | M8 |
| `SelectObjectContent` | not planned | Returns `NotImplemented` |
| S3 Object Lambda | not planned | Returns `NotImplemented` |
| Access Points | not planned | Returns `NotImplemented` |
| S3 Inventory and Analytics | not planned | Returns `NotImplemented` |
| S3 Batch Operations | not planned | Returns `NotImplemented` |
| Transfer Acceleration | not planned | Returns `NotImplemented` |
| Requester Pays | not planned | Returns `NotImplemented` |

## Authentication

| Feature | Status |
|---|---|
| SigV4 (header) | done |
| SigV4 presigned URLs | done |
| SigV4 streaming (`aws-chunked`) | done |
| STS temporary credentials | done |
| OIDC web identity | done |
| LDAP | done |
| Client certificates | done |

## IAM

| Feature | Status |
|---|---|
| Users and access keys | done |
| Groups | done |
| Service accounts | done |
| Policy evaluation (AWS IAM syntax) | done |
| Bucket policies | done |
| Anonymous and public access | done |
| STS session intersection | done |
| Condition keys | done |

## Where liteio diverges from AWS on purpose

These are choices, not gaps.

**`CreateBucket` is strict.** Create a bucket you already own and you get
`409 BucketAlreadyOwnedByYou` in every region, `us-east-1` included. AWS returns
`200` there as a historical exception, which turns a logical conflict into a
silent success. liteio does not, so a re-run that should have been a no-op shows
up as the conflict it is. If your tooling relied on the AWS behavior, see the
note in the [migration guide](/reference/migrating/).

**ACLs are normalized.** Arbitrary grantee ACLs (canonical-user, email) collapse
to the nearest canned ACL: `private`, `public-read`, `public-read-write`, or
`authenticated-read`. Fine-grained grants have no real meaning without a full AWS
account model behind them, so liteio maps rather than pretends.

**Unsupported is honest.** A deferred operation returns a proper
`501 NotImplemented` with `<Code>NotImplemented</Code>`, not a vague `400` or
`403` that sends you debugging your own request.
