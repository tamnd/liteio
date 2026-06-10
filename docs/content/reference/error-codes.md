---
title: "Error codes"
description: "S3 error codes returned by liteio and what they mean."
weight: 20
---

liteio returns S3-compatible XML error responses. Every error body looks like:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<Error>
  <Code>NoSuchBucket</Code>
  <Message>The specified bucket does not exist.</Message>
  <Resource>/my-bucket</Resource>
  <RequestId>abc123def456</RequestId>
</Error>
```

## Common error codes

| Code | HTTP status | Meaning |
|---|---|---|
| `AccessDenied` | 403 | The request signature is invalid, the credential is revoked, or the IAM policy denies the action. |
| `AuthorizationHeaderMalformed` | 400 | The `Authorization` header is present but syntactically invalid. |
| `BucketAlreadyExists` | 409 | A bucket with that name already exists and is owned by another account. |
| `BucketAlreadyOwnedByYou` | 409 | You already own a bucket with that name. |
| `BucketNotEmpty` | 409 | `DeleteBucket` was called on a non-empty bucket. |
| `EntityTooLarge` | 400 | Single-part object exceeds the 5 GB limit. |
| `EntityTooSmall` | 400 | A multipart part (other than the last) is smaller than 5 MB. |
| `ExpiredToken` | 400 | The STS session token has expired. |
| `IncompleteBody` | 400 | The request body is shorter than `Content-Length` declared. |
| `InternalError` | 500 | An unexpected server error. Check the server log for details. |
| `InvalidAccessKeyId` | 403 | The access key does not exist. |
| `InvalidArgument` | 400 | A query parameter or header value is invalid. |
| `InvalidBucketName` | 400 | The bucket name violates S3 naming rules. |
| `InvalidObjectName` | 400 | The object key is empty or otherwise invalid. |
| `InvalidPart` | 400 | One of the specified multipart parts does not exist or its ETag does not match. |
| `InvalidPartOrder` | 400 | Parts in `CompleteMultipartUpload` are not in ascending order. |
| `InvalidRange` | 416 | The `Range` header requests a range outside the object's size. |
| `InvalidSignature` | 403 | The request signature does not match. |
| `MalformedXML` | 400 | The request XML body is malformed. |
| `MethodNotAllowed` | 405 | HTTP method is not supported for this resource. |
| `MissingContentLength` | 411 | A streaming request is missing `Content-Length`. |
| `NoSuchBucket` | 404 | The specified bucket does not exist. |
| `NoSuchKey` | 404 | The specified object does not exist. |
| `NoSuchUpload` | 404 | The specified multipart upload ID does not exist or has been aborted. |
| `NoSuchVersion` | 404 | The specified object version does not exist. |
| `NotImplemented` | 501 | The requested feature is not implemented. |
| `PreconditionFailed` | 412 | A conditional request header (`If-Match`, `If-Unmodified-Since`) was not satisfied. |
| `RequestTimeTooSkewed` | 403 | The request timestamp differs from the server time by more than 15 minutes. |
| `SignatureDoesNotMatch` | 403 | The computed signature does not match the one in the request. |
| `SlowDown` | 503 | The erasure set is below read quorum or the server is overloaded. |
| `TooManyBuckets` | 400 | The account has reached the maximum allowed bucket count. |
| `XAmzContentSHA256Mismatch` | 400 | The `x-amz-content-sha256` hash does not match the request body. |
