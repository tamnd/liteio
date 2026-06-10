---
title: "Healing"
description: "How liteio finds and repairs degraded objects."
weight: 20
---

liteio repairs itself. When a drive fails, every object on it is queued for
reconstruction, and the rebuilt shards are written back when a replacement drive
appears. A routine drive failure needs no operator action beyond swapping the
disk.

## Reactive healing

When a read or write notices a missing or corrupt shard, the object goes into the
MRF queue, where MRF is "most recently failed." A background worker drains the
queue, rebuilds each object from the shards that survived, and writes the
repaired shard back. The depth of that queue is in
`liteio_cluster_heal_queue_depth` and on the console dashboard, so you can watch
a repair finish.

## Bitrot

Every shard carries a HighwayHash-256 checksum that is verified on every read. A
mismatch does two things at once: the read still succeeds, served from the other
shards, and the bad shard is queued for repair. Silent corruption never reaches
the client.

## Reading through a failure

If the number of offline drives in a set is within the parity level, reads
reconstruct the missing shards on the fly. The client gets the right bytes, the
event is logged, and the object is queued for repair. The client never sees the
degradation.

Past that point, when a set is below read quorum, reads return `503 SlowDown` and
writes are refused until drives come back. liteio would rather stall than hand
back data it cannot verify.

## Replacing a drive

1. Swap the dead disk and mount it at the same path.
2. liteio spots the empty drive on startup and starts healing onto it.
3. Watch `liteio_cluster_heal_queue_depth` fall to zero. That is "done."

The console shows per-drive health and per-set availability throughout, so you
can confirm the set is whole again before you walk away.

## Proactive scanning

A low-priority scanner walks every object across every set on a schedule,
re-checks the checksums, and repairs anything it finds. This is what catches
bitrot on a drive that is still online but quietly returning bad data, the
failure mode that reactive healing alone would miss. It runs below request
traffic so it does not show up in your latency.
