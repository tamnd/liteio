---
title: "Configuration"
description: "Every flag and environment variable liteio accepts."
weight: 40
---

liteio reads configuration from command-line flags. Every flag has a
corresponding environment variable in the form `LITEIO_<FLAG_NAME_UPPERCASED>`
with hyphens replaced by underscores.

## Core flags

| Flag | Env | Default | Description |
|---|---|---|---|
| `--address` | `LITEIO_ADDRESS` | `:9000` | S3 API listen address |
| `--console-address` | `LITEIO_CONSOLE_ADDRESS` | `:9001` | Web console listen address |
| `--drives` | `LITEIO_DRIVES` | — | Comma-separated drive paths or endpoint patterns |
| `--parity` | `LITEIO_PARITY` | drives/2 | Number of parity shards per erasure set |
| `--access-key` | `LITEIO_ACCESS_KEY` | — | Root credential access key |
| `--secret-key` | `LITEIO_SECRET_KEY` | — | Root credential secret key |

## Cluster flags

These flags are only needed in distributed mode. A single-node deployment
omits them entirely.

| Flag | Env | Default | Description |
|---|---|---|---|
| `--cluster-address` | `LITEIO_CLUSTER_ADDRESS` | — | Inter-node RPC listen address. Setting this enables distributed mode. |
| `--node-host` | `LITEIO_NODE_HOST` | — | Hostname of this node (used to detect local vs remote drives) |
| `--peers` | `LITEIO_PEERS` | — | Comma-separated cluster addresses of peer nodes |
| `--cluster-cert` | `LITEIO_CLUSTER_CERT` | — | Path to TLS certificate for inter-node mTLS |
| `--cluster-key` | `LITEIO_CLUSTER_KEY` | — | Path to TLS private key for inter-node mTLS |
| `--cluster-ca` | `LITEIO_CLUSTER_CA` | — | Path to CA certificate bundle for inter-node mTLS |
| `--cluster-server-name` | `LITEIO_CLUSTER_SERVER_NAME` | — | TLS server name used when dialing peers |

## TLS flags

| Flag | Env | Default | Description |
|---|---|---|---|
| `--tls-cert` | `LITEIO_TLS_CERT` | — | TLS certificate for the S3 API listener |
| `--tls-key` | `LITEIO_TLS_KEY` | — | TLS private key for the S3 API listener |
| `--console-tls-cert` | `LITEIO_CONSOLE_TLS_CERT` | — | TLS certificate for the console listener |
| `--console-tls-key` | `LITEIO_CONSOLE_TLS_KEY` | — | TLS private key for the console listener |
| `--console-insecure-cookie` | `LITEIO_CONSOLE_INSECURE_COOKIE` | false | Allow the console session cookie over plain HTTP (dev only) |

## Metrics flag

| Flag | Env | Default | Description |
|---|---|---|---|
| `--metrics-token` | `LITEIO_METRICS_TOKEN` | — | Bearer token required to scrape `/metrics`. No token = endpoint disabled. |

## Debug flag

| Flag | Env | Default | Description |
|---|---|---|---|
| `--debug-address` | `LITEIO_DEBUG_ADDRESS` | — | Address to serve `net/http/pprof` handlers for CPU and memory profiling |

## Drive patterns

In distributed mode, `--drives` accepts endpoint patterns using brace expansion:

```
https://node{1...4}.example.com:9100/mnt/disk{1...8}
```

This expands to 32 drive endpoints (4 nodes × 8 disks). liteio distributes them
across erasure sets automatically based on the total drive count and the
configured parity level.

A drive endpoint is local to the node when its hostname matches `--node-host`.
Remote drives are served over the inter-node RPC transport; local drives use
direct `O_DIRECT` I/O.

## Environment-only settings

A few low-level knobs are only exposed as environment variables.

| Env | Description |
|---|---|
| `LITEIO_GO_MAX_PROCS` | Overrides `GOMAXPROCS`. Defaults to the number of available CPUs. |

## Example: single node

```bash
liteio \
  --address :9000 \
  --drives /mnt/d1,/mnt/d2,/mnt/d3,/mnt/d4 \
  --parity 2 \
  --access-key admin \
  --secret-key changeme \
  --metrics-token secret-scrape-token \
  --tls-cert /etc/liteio/server.crt \
  --tls-key /etc/liteio/server.key
```

## Example: environment-only

All flags can be set from the environment, which is useful in containerized
deployments where you want to avoid shell quoting:

```bash
export LITEIO_ADDRESS=:9000
export LITEIO_DRIVES=/mnt/d1,/mnt/d2,/mnt/d3,/mnt/d4
export LITEIO_ACCESS_KEY=admin
export LITEIO_SECRET_KEY=changeme
liteio
```
