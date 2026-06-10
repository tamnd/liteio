---
title: "Configuration"
description: "Every flag and environment variable liteio reads."
weight: 40
---

liteio is configured with command-line flags. Every flag has a matching
environment variable: take the flag name, uppercase it, replace hyphens with
underscores, and prefix `LITEIO_`. So `--access-key` is `LITEIO_ACCESS_KEY`.
Flags win over the environment when both are set.

## Core

| Flag | Environment | Default | Purpose |
|---|---|---|---|
| `--address` | `LITEIO_ADDRESS` | `:9000` | S3 API listen address |
| `--console-address` | `LITEIO_CONSOLE_ADDRESS` | `:9001` | Web console listen address |
| `--drives` | `LITEIO_DRIVES` | required | Comma-separated drive paths or endpoint patterns |
| `--parity` | `LITEIO_PARITY` | drives / 2 | Parity shards per erasure set |
| `--access-key` | `LITEIO_ACCESS_KEY` | required | Root credential access key |
| `--secret-key` | `LITEIO_SECRET_KEY` | required | Root credential secret key |

`--parity` is the durability knob. The default, half the drives in a set, is the
most durable split and lets you lose half the set without data loss. Lower it to
trade redundancy for usable capacity.

## Cluster

These only apply in distributed mode. A single node ignores them. Setting
`--cluster-address` is what turns distributed mode on.

| Flag | Environment | Purpose |
|---|---|---|
| `--cluster-address` | `LITEIO_CLUSTER_ADDRESS` | Inter-node RPC listen address (enables distributed mode) |
| `--node-host` | `LITEIO_NODE_HOST` | This node's hostname, used to tell local drives from remote |
| `--peers` | `LITEIO_PEERS` | Comma-separated cluster addresses of the other nodes |
| `--cluster-cert` | `LITEIO_CLUSTER_CERT` | TLS certificate for inter-node mTLS |
| `--cluster-key` | `LITEIO_CLUSTER_KEY` | TLS private key for inter-node mTLS |
| `--cluster-ca` | `LITEIO_CLUSTER_CA` | CA bundle for inter-node mTLS |
| `--cluster-server-name` | `LITEIO_CLUSTER_SERVER_NAME` | TLS server name used when dialing peers |

## TLS

| Flag | Environment | Purpose |
|---|---|---|
| `--tls-cert` | `LITEIO_TLS_CERT` | Certificate for the S3 API listener |
| `--tls-key` | `LITEIO_TLS_KEY` | Private key for the S3 API listener |
| `--console-tls-cert` | `LITEIO_CONSOLE_TLS_CERT` | Certificate for the console listener |
| `--console-tls-key` | `LITEIO_CONSOLE_TLS_KEY` | Private key for the console listener |
| `--console-insecure-cookie` | `LITEIO_CONSOLE_INSECURE_COOKIE` | Allow the console session cookie over plain HTTP. Local testing only. |

## Metrics and debug

| Flag | Environment | Purpose |
|---|---|---|
| `--metrics-token` | `LITEIO_METRICS_TOKEN` | Bearer token to scrape `/metrics`. With no token, the endpoint is not served at all. |
| `--debug-address` | `LITEIO_DEBUG_ADDRESS` | Address for the `net/http/pprof` handlers. Leave unset in production. |

## Drive patterns

In distributed mode, `--drives` takes endpoint patterns with brace expansion:

```
https://node{1...4}.example.com:9100/mnt/disk{1...8}
```

That expands to 32 endpoints, four nodes by eight disks. liteio lays them out
across erasure sets based on the total count and `--parity`. A drive is local to
the node whose `--node-host` matches its hostname; everything else is reached
over the inter-node RPC transport.

## Two ways to write the same config

As flags:

```bash
liteio \
  --address :9000 \
  --drives /mnt/d1,/mnt/d2,/mnt/d3,/mnt/d4 \
  --parity 2 \
  --access-key admin \
  --secret-key changeme \
  --metrics-token scrape-token
```

As environment, which is friendlier inside containers where shell quoting bites:

```bash
export LITEIO_ADDRESS=:9000
export LITEIO_DRIVES=/mnt/d1,/mnt/d2,/mnt/d3,/mnt/d4
export LITEIO_ACCESS_KEY=admin
export LITEIO_SECRET_KEY=changeme
liteio
```
