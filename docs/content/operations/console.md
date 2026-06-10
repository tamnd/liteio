---
title: "Web console"
description: "The built-in web console for cluster management."
weight: 30
---

The console is a single-page app served straight from the binary on the console
port, default `:9001`. There is no second process to deploy. Sign in with the
root credential or any IAM user.

## What it does

**Cluster dashboard.** Topology map, drive reachability, per-set quorum and
availability, raw and usable capacity, and the heal queue depth, all live.

**Bucket browser.** Create and delete buckets, walk the object tree, upload,
download, and delete objects or whole prefixes.

**Identity.** Create users, set passwords, attach policies. Build groups and add
members. Mint per-user service accounts with narrowed policies.

**Policy editor.** Write and edit IAM policies in a JSON editor, then attach or
detach them from users and groups.

**Metrics.** Live charts for request rate, error rate, latency percentiles, and
cluster health, drawn from the same data the Prometheus endpoint serves.

## How it stays safe

The console uses server-side sessions. Your secret key never reaches the browser.
Every admin action is signed in-process against the admin API with the
session's credential, so there is nothing sensitive sitting in client-side
storage to steal.

For local testing over plain HTTP, add `--console-insecure-cookie` so the
session cookie is accepted without TLS. In production the console belongs behind
TLS, and without that flag it insists on it.

## Reaching it

```
http://localhost:9001
```

Sign in with the access key as the username and the secret key as the password.

To terminate TLS in liteio, give the console listener its own certificate, which
can be the same one the S3 listener uses:

```bash
liteio \
  --console-address :9001 \
  --console-tls-cert /etc/liteio/server.crt \
  --console-tls-key  /etc/liteio/server.key
```
