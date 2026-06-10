---
title: "Error codes"
description: "The S3 error codes liteio returns and what to do about them."
weight: 20
---

liteio returns S3-shaped XML errors. Every body has the same envelope, so your
existing error handling parses them unchanged:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>NoSuchBucket</Code>
  <Message>The specified bucket does not exist.</Message>
  <Resource>/my-bucket</Resource>
  <RequestId>abc123def456</RequestId>
</Error>
```

The `RequestId` is worth logging on the client side; it lines up with the
server log entry for the same request when you need to trace one down.

## The catalog

| Code | HTTP | What it means |
|---|---|---|
| `AccessDenied` | 403 | Bad signature, revoked credential, or an IAM policy that denies the action. |
| `AuthorizationHeaderMalformed` | 400 | The `Authorization` header is present but malformed. |
| `BucketAlreadyExists` | 409 | The name is taken by another account. |
| `BucketAlreadyOwnedByYou` | 409 | You already own a bucket by this name. |
| `BucketNotEmpty` | 409 | You tried to delete a bucket with objects in it. |
| `EntityTooLarge` | 400 | A single-part object went over the 5 GB ceiling. |
| `EntityTooSmall` | 400 | A multipart part other than the last is under 5 MB. |
| `ExpiredToken` | 400 | The STS session token has expired. |
| `IncompleteBody` | 400 | The body was shorter than the declared `Content-Length`. |
| `InternalError` | 500 | An unexpected server fault. The server log has the detail. |
| `InvalidAccessKeyId` | 403 | No such access key. |
| `InvalidArgument` | 400 | A header or query parameter value is invalid. |
| `InvalidBucketName` | 400 | The bucket name breaks the S3 naming rules. |
| `InvalidObjectName` | 400 | The object key is empty or otherwise invalid. |
| `InvalidPart` | 400 | A listed multipart part is missing or its ETag does not match. |
| `InvalidPartOrder` | 400 | Parts in `CompleteMultipartUpload` are not in ascending order. |
| `InvalidRange` | 416 | The `Range` falls outside the object. |
| `InvalidSignature` | 403 | The signature does not match. |
| `MalformedXML` | 400 | The request XML does not parse. |
| `MethodNotAllowed` | 405 | That method is not valid for this resource. |
| `MissingContentLength` | 411 | A streaming request came in without `Content-Length`. |
| `NoSuchBucket` | 404 | The bucket does not exist. |
| `NoSuchKey` | 404 | The object does not exist. |
| `NoSuchUpload` | 404 | The multipart upload ID is unknown or already aborted. |
| `NoSuchVersion` | 404 | The object version does not exist. |
| `NotImplemented` | 501 | The feature is not implemented. See the compatibility matrix. |
| `PreconditionFailed` | 412 | A conditional header (`If-Match`, `If-Unmodified-Since`) was not met. |
| `RequestTimeTooSkewed` | 403 | The request clock is more than 15 minutes off the server. |
| `SignatureDoesNotMatch` | 403 | The computed signature does not match the request. |
| `SlowDown` | 503 | A set is below read quorum, or the server is shedding load. Back off and retry. |
| `TooManyBuckets` | 400 | The account hit the bucket-count limit. |
| `XAmzContentSHA256Mismatch` | 400 | The body hash did not match `x-amz-content-sha256`. |

## Two you will actually hit

`RequestTimeTooSkewed` almost always means the client clock has drifted. Run NTP
on the client before you suspect liteio.

`SlowDown` is backpressure, not a bug. The right response is exponential backoff,
which every AWS SDK already does for you. A steady stream of it means a set is
degraded; check `liteio_cluster_sets_below_read_quorum` in
[metrics](/operations/metrics/).
