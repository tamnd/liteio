---
title: "Encryption"
description: "Server-side encryption with customer-provided keys (SSE-C)."
weight: 40
---

liteio supports SSE-C (server-side encryption with customer-provided keys).
The client supplies the encryption key on each request; liteio encrypts or
decrypts the object and then discards the key. The key is never stored.

## How SSE-C works

1. The client generates a 256-bit AES key.
2. The client sends the key in the `x-amz-server-side-encryption-customer-key`
   header (base64-encoded) and its MD5 in
   `x-amz-server-side-encryption-customer-key-MD5`.
3. liteio verifies the MD5, encrypts the object with AES-256-CTR, stores the
   key MD5 and a random nonce in the object metadata, and discards the key.
4. On GET and HEAD, the client supplies the same key. liteio verifies it
   against the stored MD5, decrypts in-flight, and streams the plaintext.

Because the key is not stored, liteio cannot decrypt the object without the
client providing the key. Losing the key means losing the data.

## Upload with SSE-C

```bash
# Generate a random 32-byte key.
KEY=$(openssl rand -base64 32)
KEY_MD5=$(echo -n "$KEY" | base64 -d | openssl md5 -binary | base64)

aws s3api put-object \
  --bucket my-bucket \
  --key secret.txt \
  --body secret.txt \
  --sse-customer-algorithm AES256 \
  --sse-customer-key "$KEY" \
  --sse-customer-key-md5 "$KEY_MD5" \
  --profile liteio
```

## Download with SSE-C

Supply the same key:

```bash
aws s3api get-object \
  --bucket my-bucket \
  --key secret.txt \
  output.txt \
  --sse-customer-algorithm AES256 \
  --sse-customer-key "$KEY" \
  --sse-customer-key-md5 "$KEY_MD5" \
  --profile liteio
```

Without the correct key, the server returns `403 AccessDenied`.

## HEAD with SSE-C

```bash
aws s3api head-object \
  --bucket my-bucket \
  --key secret.txt \
  --sse-customer-algorithm AES256 \
  --sse-customer-key "$KEY" \
  --sse-customer-key-md5 "$KEY_MD5" \
  --profile liteio
```

## Copy with SSE-C

Both source and destination keys are required when copying an encrypted object:

```bash
aws s3api copy-object \
  --bucket dest-bucket \
  --key copy-of-secret.txt \
  --copy-source my-bucket/secret.txt \
  --copy-source-sse-customer-algorithm AES256 \
  --copy-source-sse-customer-key "$SOURCE_KEY" \
  --copy-source-sse-customer-key-md5 "$SOURCE_KEY_MD5" \
  --sse-customer-algorithm AES256 \
  --sse-customer-key "$DEST_KEY" \
  --sse-customer-key-md5 "$DEST_KEY_MD5" \
  --profile liteio
```

## TLS requirement

SSE-C is only safe over HTTPS. The key travels in an HTTP header; plain HTTP
exposes it on the wire. Always terminate TLS before sending SSE-C requests.
