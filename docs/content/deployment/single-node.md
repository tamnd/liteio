---
title: "Single node"
description: "Run liteio on one server with local drives."
weight: 10
---

A single node is the simplest production setup. Every drive is local, there is
no inter-node RPC, and you leave the cluster flags off entirely. This is the
right choice until you need to survive a whole machine going down.

## Pick the drives

Give liteio one directory per physical drive. Mapping directories to real disks
is what lets it detect and heal a single failed drive instead of losing
everything when one disk dies.

```
/mnt/drive1
/mnt/drive2
/mnt/drive3
/mnt/drive4
```

More drives buys you two things at once: parallel I/O for throughput, and more
shards to lose before data is gone. Parity defaults to half the drives, the most
durable split. Drop it with `--parity` if you would rather spend the space on
capacity.

## Run it

A production invocation with TLS terminated in liteio itself:

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

On first run, liteio formats the drives and writes a `format.json` manifest to
each. On later runs it reads those manifests and refuses to start if the drive
set has drifted, which catches a mis-mounted disk before it becomes a data
problem.

## Run it under systemd

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

The high `LimitNOFILE` matters: liteio holds a file handle per open drive shard,
and the default 1024 runs out under load.

## Put it behind a reverse proxy

If you would rather terminate TLS at nginx or Caddy, bind liteio to loopback and
proxy to it. Two settings are non-negotiable: turn buffering off so streaming
uploads are not spooled to disk, and remove the body size cap so large objects
go through.

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

With the proxy handling TLS, drop liteio's `--tls-cert` and `--tls-key`.

## Scrape it with Prometheus

The token below must match `--metrics-token`. With no token configured, the
endpoint is not served, so this is opt-in.

```yaml
scrape_configs:
  - job_name: liteio
    bearer_token: scrape-token
    static_configs:
      - targets: [localhost:9001]
    metrics_path: /metrics
```

See [Metrics](/operations/metrics/) for the full catalog.
