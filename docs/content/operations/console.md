---
title: "Web console"
description: "The embedded web console for cluster management."
weight: 30
---

liteio ships an embedded web console on the console port (default `:9001`).
Sign in with any IAM user or the root credential. The console is a single-page
application served directly from the binary — no separate process needed.

## Features

**Cluster dashboard** — topology map, drive reachability, per-set quorum and
availability, raw and usable capacity, and the reactive heal queue depth.

**Bucket browser** — create, delete, and browse buckets. Navigate the object
tree, upload files, download objects, and delete individual objects or
prefixes.

**Identity management** — create users, set passwords, attach policies. Create
groups and assign members. Create per-user service accounts with narrowed
policies.

**Policy editor** — create and edit IAM policies with a JSON editor. Attach
and detach policies from users and groups.

**Metrics** — live charts for request rate, error rate, latency percentiles,
and cluster health. The data comes from the same Prometheus endpoint the
scraper uses.

## Security

The console uses server-side sessions. The secret key is never stored in the
browser. Every admin action is signed in-process against the admin API using
the session-bound credential. Behind plain HTTP for local testing, add
`--console-insecure-cookie`; in production the console must sit behind TLS.

## Accessing the console

```
http://localhost:9001
```

Sign in with:
- Username: the access key (e.g. `admin`)
- Password: the secret key (e.g. `changeme`)

## TLS for the console

Pass separate TLS flags for the console listener:

```bash
liteio \
  --console-address :9001 \
  --console-tls-cert /etc/liteio/server.crt \
  --console-tls-key  /etc/liteio/server.key
```

You can share the same certificate as the S3 API listener.
