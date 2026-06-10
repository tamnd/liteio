---
title: "Distributed cluster"
description: "Run liteio across multiple nodes for fault tolerance and horizontal scale."
weight: 20
---

A distributed liteio cluster spans multiple nodes. Each node runs the same
binary with the same flags, differing only in `--node-host`. The cluster has no
primary node; any node can serve any request.

## Cluster topology

The `--drives` flag in distributed mode accepts endpoint patterns with brace
expansion. This example has four nodes, each with eight drives:

```
https://node{1...4}.example.com:9100/mnt/disk{1...8}
```

This expands to 32 drive endpoints. liteio distributes them into erasure sets
based on the drive count and the parity level. With `--parity 4`, each set
holds 8 drives (4 data + 4 parity), giving four erasure sets total.

## Run each node

Every node runs the same command with its own `--node-host`:

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

Replace `node1.example.com` with each node's actual hostname. The `--peers`
list should include the other three nodes' cluster addresses. The node running
the command is not listed in `--peers`.

## mTLS for inter-node traffic

Inter-node RPC traffic carries object data and namespace locks. Protect it with
mutual TLS:

1. Generate a cluster CA and per-node certificates signed by it.
2. Pass `--cluster-cert`, `--cluster-key`, and `--cluster-ca` to each node.
3. Set `--cluster-server-name` to the value used in the server certificates'
   CN or SAN fields.

On a trusted private network you can omit the TLS flags entirely. The
cluster-address listener then speaks plain HTTP.

## Cluster bring-up

On first run, each node waits for a quorum of peers to come online before
formatting. Once a quorum is present, every node writes its `format.json`
manifest and the cluster is live.

To add a new server pool later, expand `--drives` with the new pool's drive
endpoints and restart all nodes. liteio detects the additional pool, writes a
new `format.json` to the new drives, and begins placing objects on the new
capacity immediately.

## Failure modes

| Scenario | Outcome |
|---|---|
| Single drive offline | Reads reconstruct from remaining shards. Reactive healer queues the object for repair when the drive returns. |
| Single node offline | Reads and writes continue through other nodes serving the affected drives. Namespace locks are quorum-based: a majority of nodes must agree. |
| Split brain (network partition) | The minority partition refuses writes. Reads from the minority return stale data. The majority partition continues normally. |
| Below read quorum | Reads return `503 SlowDown`. Writes are rejected. The cluster waits for drives to return. |

## Health checks

The admin API on the console port exposes cluster health:

```bash
curl -s http://node1.example.com:9001/minio/health/live
# 200 OK: node is alive
curl -s http://node1.example.com:9001/minio/health/cluster
# 200 OK: cluster has full quorum
# 503: degraded or below read quorum
```

These endpoints are unauthenticated and safe to use in a load-balancer health
check.
