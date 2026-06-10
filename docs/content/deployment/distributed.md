---
title: "Distributed cluster"
description: "Run liteio across machines for fault tolerance and scale."
weight: 20
---

A distributed cluster spans several nodes. Each runs the same binary with the
same flags, differing only in `--node-host`. There is no primary and no
coordinator process; any node serves any request, and the cluster keeps going
when a node drops.

## Lay out the drives

In distributed mode `--drives` takes endpoint patterns with brace expansion.
This is four nodes with eight drives each:

```
https://node{1...4}.example.com:9100/mnt/disk{1...8}
```

That is 32 drive endpoints. liteio packs them into erasure sets from the drive
count and the parity level. With `--parity 4` each set is eight drives, four for
data and four for parity, giving four sets across the cluster.

## Start each node

Every node runs the same command. Only `--node-host` changes per machine, and a
node never lists itself in `--peers`.

```bash
# On node1:
liteio \
  --address :9000 \
  --cluster-address :9100 \
  --node-host node1.example.com \
  --drives 'https://node{1...4}.example.com:9100/mnt/disk{1...8}' \
  --peers https://node2.example.com:9100,https://node3.example.com:9100,https://node4.example.com:9100 \
  --parity 4 \
  --access-key admin \
  --secret-key changeme \
  --cluster-cert /etc/liteio/node1.crt \
  --cluster-key  /etc/liteio/node1.key \
  --cluster-ca   /etc/liteio/cluster-ca.crt \
  --cluster-server-name liteio-cluster
```

## Protect inter-node traffic

The cluster address carries object data and namespace locks between nodes. On
anything but a fully trusted private network, wrap it in mutual TLS:

1. Make a cluster CA and a certificate per node, signed by that CA.
2. Pass `--cluster-cert`, `--cluster-key`, and `--cluster-ca` to every node.
3. Set `--cluster-server-name` to the name in the certificates' CN or SAN.

On a trusted network you can leave all of that off and the cluster listener
speaks plain HTTP.

## Bring-up

On a fresh cluster, each node waits for a quorum of peers before it formats
anything. Once enough nodes are present, they all write their `format.json`
manifests together and the cluster goes live. Starting the nodes in any order is
fine; they rendezvous on quorum.

To grow later, add a new pool's drive endpoints to `--drives` and restart the
nodes. liteio sees the extra pool, formats the new drives, and starts placing
objects on the new capacity right away. Existing data stays where it is.

## What happens when things break

| Situation | What liteio does |
|---|---|
| One drive offline | Reads reconstruct from the surviving shards. The healer queues the object and repairs it when the drive returns. |
| One node offline | Other nodes keep serving its drives. Namespace locks need a majority of nodes to agree, so a single loss is transparent. |
| Network partition | The minority side stops accepting writes. The majority side runs normally. Reads from the minority may be stale. |
| Below read quorum | Reads return `503 SlowDown` and writes are rejected until enough drives come back. liteio refuses to serve data it cannot verify. |

## Health checks

The console port exposes unauthenticated health endpoints, safe to wire into a
load balancer:

```bash
curl -s http://node1.example.com:9001/minio/health/live
# 200: this node is up

curl -s http://node1.example.com:9001/minio/health/cluster
# 200: full quorum
# 503: degraded or below read quorum
```

Point the balancer at `/minio/health/cluster` to drain a node that has lost
quorum, and at `/minio/health/live` for a plain liveness probe.
