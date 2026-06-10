---
title: "Single node"
description: "Run liteio on one server with local drives."
weight: 10
---

A single-node deployment is the simplest production configuration. All drives
are local, there is no inter-node RPC, and the cluster-address flag is omitted.

## Choose drives

Give liteio at least two directories, one per physical drive. Using one
directory per physical disk lets liteio detect and recover from individual drive
failures.

```
/mnt/drive1
/mnt/drive2
/mnt/drive3
/mnt/drive4
```

More drives improve both performance (parallel I/O) and durability (more shards
to lose before data is unrecoverable). The minimum parity level is 1 (1 parity
shard for N-1 data shards). The default is N/2 — the most durable split.

## Run liteio

A minimal production invocation with four drives and TLS:

```bash
liteio \
  --address :443 \
  --drives /mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4 \
  --parity 2 \
  --access-key admin \
  --secret-key changeme \
  --tls-cert /etc/liteio/server.crt \
  --tls-key  /etc/liteio/server.key \
  --console-address :9001 \
  --console-tls-cert /etc/liteio/server.crt \
  --console-tls-key  /etc/liteio/server.key \
  --metrics-token scrape-token
```

liteio formats the drives on first run and writes a `format.json` manifest to
each drive. Subsequent runs read the manifest and validate that the drive set is
consistent.

## Systemd unit

```ini
[Unit]
Description=liteio object store
After=network.target

[Service]
ExecStart=/usr/local/bin/liteio \
  --address :9000 \
  --drives /mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4 \
  --parity 2 \
  --access-key admin \
  --secret-key changeme
Restart=on-failure
RestartSec=5s
LimitNOFILE=65536

[Install]
WantedBy=multi-user.target
```

```bash
systemctl enable --now liteio
```

## Reverse proxy

liteio speaks plain HTTP on the data path. To terminate TLS at a reverse proxy
instead of in liteio itself, point nginx or Caddy at `:9000`:

```nginx
server {
    listen 443 ssl;
    server_name s3.example.com;

    ssl_certificate     /etc/certs/server.crt;
    ssl_certificate_key /etc/certs/server.key;

    location / {
        proxy_pass http://127.0.0.1:9000;
        proxy_set_header Host $host;
        proxy_buffering off;
        client_max_body_size 0;
    }
}
```

With a reverse proxy handling TLS, omit the `--tls-cert` / `--tls-key` flags
from liteio and bind it on a loopback address.

## Prometheus scraping

Add a scrape target to your Prometheus config. The token must match
`--metrics-token`:

```yaml
scrape_configs:
  - job_name: liteio
    bearer_token: scrape-token
    static_configs:
      - targets: [localhost:9001]
    metrics_path: /metrics
```

Metrics include per-API request counts, error counts by S3 code, latency
histograms, drive health, erasure set availability, and heap / GC stats.
