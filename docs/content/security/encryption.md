---
title: "Encryption"
description: "Server-side encryption with customer-provided keys (SSE-C)."
weight: 40
---

With SSE-C, you hold the key. You send it on each request, liteio uses it to
encrypt or decrypt the object in flight, then throws it away. The key is never
written to disk.

## How it works

1. You generate a 256-bit AES key.
2. You send the key in `x-amz-server-side-encryption-customer-key`
   (base64-encoded) and its MD5 in the matching `-key-MD5` header.
3. liteio checks the MD5, encrypts with AES-256-CTR, stores only the key's MD5
   and a random nonce in the object metadata, and discards the key.
4. On GET or HEAD you send the same key. liteio matches it against the stored
   MD5, decrypts on the fly, and streams the plaintext.

Because liteio keeps the MD5 and not the key, it cannot read your object without
you. The flip side is the obvious one: lose the key and the data is gone. There
is no recovery path, by design.

## Upload

```bash
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

## Download

Same key, or no data:

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

A wrong or missing key gets `403 AccessDenied`. HEAD takes the same three
headers.

## Copy

Copying an encrypted object needs the source key to read and the destination key
to re-encrypt:

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

> SSE-C only makes sense over HTTPS. The key travels in a plain HTTP header, so
> on an unencrypted connection it is exposed on the wire and the whole exercise
> is pointless. Terminate TLS before you send one.
