---
title: "liteio"
description: "A fast, fully open, S3-compatible object store written in Go."
---

MinIO's community edition went into maintenance mode in December 2025 and moved
its management features behind a commercial product. liteio is the answer to
that: an S3-compatible object store that ships every feature for free under
Apache-2.0, runs as one static binary, and is engineered to saturate the
hardware you paid for.

It speaks the AWS S3 API that your SDKs, `aws` CLI, `mc`, and `rclone` already
know. Point a client at it and it behaves like S3. Underneath, an object's
location is computed from its name, so there is no metadata database to run,
replicate, or lose.

A few numbers from a 6-vCPU VPS with 10,000 objects in a bucket:

| Operation | liteio | Garage (same box) |
| --- | --- | --- |
| List 1,000 objects (p50) | 32 ms | 195 ms |
| GET small object (p50) | 19 ms | |
| PUT small object (p50) | 149 ms | |

New here? The [quick start](/getting-started/quick-start/) gets you a live S3
endpoint in two minutes with nothing but a Go toolchain. Coming from MinIO? The
[migration guide](/reference/migrating/) is usually a one-line endpoint change.
Want the honest list of what works today? Read the
[compatibility matrix](/reference/compatibility/).
