---
title: "Introduction"
description: "What liteio is, why it exists, and how it is different from MinIO."
weight: 10
---

liteio is an S3-compatible object store written in Go. It is a ground-up
alternative to MinIO, designed for the world after MinIO's community edition
entered maintenance mode in December 2025 and moved management features behind
the commercial AIStor product.

## Why liteio

liteio optimizes for three things in priority order.

**Performance.** The data path avoids unnecessary copies. Reads use `O_DIRECT`,
Reed-Solomon coding runs with SIMD acceleration, and the in-memory namespace
index cuts `ListObjectsV2` from seconds to milliseconds even on buckets with
millions of objects. On server2 (6 vCPU, 11 GiB, network-attached block
storage) liteio lists 1,000 objects in 32 ms — roughly 6x faster than Garage on
the same hardware.

**Permissive and complete.** Apache-2.0, no open-core feature gating, and a full
free web console: bucket management, IAM, lifecycle, tagging, replication, and
tiering. Every feature that MinIO removed from its community edition is
first-class and free here.

**Operational simplicity.** A single static binary with sane defaults. No
external database, no metadata server, no etcd or ZooKeeper for the core data
path. The placement of every object is computed from its name; the server never
looks it up.

## The S3 contract

The external contract is the AWS S3 REST API as spoken by the AWS SDKs, the
`aws s3` / `aws s3api` CLI, `mc`, `rclone`, `boto3`, and the broader S3
ecosystem. An unmodified S3 client pointed at a liteio endpoint behaves as it
does against AWS S3 for the operations in scope:

- SigV4 authentication (header, presigned URLs, and streaming `aws-chunked`).
- The XML request and response shapes, including the error-code catalog.
- Multipart upload semantics and the `ETag` / `x-amz-*` header contract.
- Versioning, delete markers, and `ListObjectVersions`.
- Bucket and object tagging, lifecycle configuration, and access policies.

A few deliberate divergences exist and are documented on the
[compatibility page](/reference/compatibility/).

## How placement works

liteio uses a three-tier topology: cluster → server pool → erasure set → drives.
An object's location is computed from its name using two hash functions; the
server never maintains a placement database.

1. A CRC32 over the object key, weighted by pool free space, selects the server
   pool.
2. A SipHash-2-4 keyed on the deployment ID selects the erasure set within that
   pool.
3. A salted permutation of the drive list determines which drive holds which
   erasure shard.

Reads recompute the same three functions and go straight to the drives. This
means a cold restart with the same drives instantly knows where every object
lives, with no warm-up or rebuild phase.

## Next steps

- [Installation](/getting-started/installation/) — build from source or download
  a binary.
- [Quick start](/getting-started/quick-start/) — a single-node server and your
  first S3 request in two minutes.
- [Configuration](/getting-started/configuration/) — every flag and environment
  variable.
