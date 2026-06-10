---
title: "Introduction"
description: "What liteio is, why it exists, and how it differs from MinIO."
weight: 10
---

liteio is an S3-compatible object store written in Go. It is a clean-room
alternative to MinIO, built after MinIO's community edition entered maintenance
mode in December 2025 and its management features moved behind the commercial
AIStor product.

## What you get

A single static binary that speaks the AWS S3 REST API and ships every
management feature for free: bucket lifecycle, IAM, policies, tagging,
versioning, federation, encryption, and a web console. Nothing is gated. The
binary you download in the free tier is the whole product.

There is no external database, no metadata server, and no etcd or ZooKeeper in
the data path. An object's location is computed from its name with two hash
functions, so a cold restart with the same drives instantly knows where every
object lives. No rebuild, no warm-up.

## Why it is fast

Speed is the first design priority, and the tradeoffs reflect that. When a
choice pits raw throughput against a knob or a feature, throughput wins and the
feature becomes opt-in.

- Reads use `O_DIRECT` and serve bytes with as few copies as the kernel allows.
- Erasure coding runs Reed-Solomon over GF(2^8) with SIMD acceleration.
- Small objects store their metadata inline, so a GET is one seek, not two.
- An in-memory namespace index answers `ListObjectsV2` without touching disk.

That last one is worth a number. On a 6-vCPU VPS, listing 1,000 objects from a
bucket of 10,000 takes 32 ms end to end. Garage on the same box takes 195 ms.
The server-side scan itself is under a millisecond; the rest is network and
scheduler.

## The S3 contract

The external contract is the AWS S3 REST API as spoken by the AWS SDKs, the
`aws s3` and `aws s3api` CLIs, `mc`, `rclone`, `boto3`, and the s3fs ecosystem.
An unmodified client pointed at liteio behaves the way it does against AWS for
every operation in scope:

- SigV4 signing: header, presigned URL, and streaming `aws-chunked`.
- The XML request and response shapes, down to the error-code catalog.
- Multipart upload semantics and the `ETag` and `x-amz-*` header contract.
- Versioning, delete markers, and `ListObjectVersions`.
- Object and bucket tagging, lifecycle rules, and access policies.

A handful of behaviors diverge from AWS on purpose. They are listed in the
[compatibility matrix](/reference/compatibility/), each with the reasoning.

## How placement works

liteio uses a three-tier topology: a cluster holds server pools, a pool holds
erasure sets, and a set is a fixed group of drives spread across nodes for
failure independence.

```
Cluster
  Server Pool 0          a unit of capacity; added to grow
    Erasure Set 0        a fixed group of N drives; the unit of redundancy
      drive 0 .. N-1     spread across nodes for failure independence
    Erasure Set 1
  Server Pool 1          a later expansion; its own erasure sets
```

Three functions turn a key into a location, and the server never stores the
result:

1. A free-space-weighted CRC32 over the key picks the server pool.
2. A SipHash-2-4 keyed on the deployment ID picks the erasure set.
3. A salted permutation decides which drive in the set holds which shard.

Reads recompute the same three functions and go straight to the drives. There is
no placement table to consult or keep consistent, which is what makes
single-node and a 100-node cluster the same code path.

Data is protected by erasure coding: an object splits into K data shards and M
parity shards across the set, and any K of the N reconstruct it. Every shard
carries a HighwayHash checksum that is verified on read, with healing on
mismatch.

## Next steps

- [Quick start](/getting-started/quick-start/): a live endpoint and your first
  object in two minutes.
- [Installation](/getting-started/installation/): build from source or grab a
  release binary.
- [Configuration](/getting-started/configuration/): every flag and environment
  variable.
