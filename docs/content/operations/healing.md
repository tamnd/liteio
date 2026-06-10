---
title: "Healing"
description: "How liteio detects and repairs degraded objects."
weight: 20
---

liteio heals data automatically. When a drive goes offline, the reactive healer
queues every object on that drive for reconstruction. When the drive returns, the
reconstructed shards are written back. No manual intervention is needed for
typical drive failures.

## Reactive healing

When a read or write detects a missing or corrupt shard, the object is queued in
the MRF (Most Recently Failed) heal queue. A background goroutine drains the
queue, reconstructs each object from its surviving shards, and writes the
repaired shard back to the drive.

The queue depth is reported in the `liteio_cluster_heal_queue_depth` metric and
visible in the console cluster dashboard.

## Bitrot detection

Every shard carries a HighwayHash-256 checksum. The checksum is verified on
every read. A checksum mismatch triggers both an error to the client (the
object is reconstructed from other shards) and an immediate healing entry.

## Read reconstruction

If the number of offline drives in an erasure set is at or below the parity
level (that is, enough data shards survive), reads transparently reconstruct
the missing shards. The client receives the correct object; the event is logged
and the object is queued for repair.

If too many drives are offline (below read quorum), the read fails with
`503 SlowDown`. Writes are also blocked until quorum is restored.

## Drive replacement

1. Replace the failed physical drive and mount it at the same path.
2. liteio detects the new empty drive on startup and begins healing.
3. Monitor progress: `liteio_cluster_heal_queue_depth` drops to zero when
   healing is complete.
4. The console cluster view shows each drive's health status and per-set
   availability.

## Proactive healing

A periodic scanner walks all objects across all erasure sets, verifies their
checksums, and repairs any degradation found. This catches silent bitrot on
drives that remain online but return corrupt data.

The scanner runs at low priority to avoid impacting request latency. Its
frequency can be tuned through the admin API.
