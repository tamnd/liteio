---
title: "Migrating from MinIO"
description: "Move an existing MinIO deployment onto liteio."
weight: 30
---

liteio targets drop-in compatibility with MinIO's S3 API. For most clients the
migration is a one-line endpoint change. Data and IAM take a bit more, and the
steps below cover both.

## Repoint the clients

No SDK changes. Swap the endpoint:

```bash
# Before
aws --endpoint-url http://minio.example.com:9000 s3 ls
# After
aws --endpoint-url http://liteio.example.com:9000 s3 ls
```

```bash
# mc
mc alias set liteio http://liteio.example.com:9000 access secret
```

## Move the data

`mc mirror` copies objects incrementally and preserves metadata and ETags. Point
it from the live MinIO at liteio:

```bash
mc alias set src  http://minio.example.com:9000  minio-access  minio-secret
mc alias set dest http://liteio.example.com:9000 liteio-access liteio-secret

mc mirror src/ dest/ --preserve
```

For a zero-downtime cutover, run it with `--watch` during the migration window so
it keeps draining the delta, then flip DNS or your load balancer to liteio once
the two sides are caught up. For a large dataset, run several `mc mirror`
processes in parallel, each scoped to a different bucket or prefix.

## Move the IAM

The model is compatible, so policies and users carry over.

Export from MinIO:

```bash
mc admin policy list myminio
mc admin policy info myminio readwrite > readwrite.json
```

Import into liteio:

```bash
mc admin policy create liteio readwrite readwrite.json
```

Re-create users and attach their policies. Secret keys cannot be exported from
MinIO, so set new ones here:

```bash
mc admin user   add    liteio alice alicepw
mc admin policy attach liteio readwrite --user alice
```

## Translate the config

| MinIO | liteio |
|---|---|
| `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` | `--access-key` / `--secret-key` |
| `MINIO_VOLUMES` | `--drives` |
| `MINIO_ERASURE_SET_DRIVE_COUNT` | not set directly; derived from drive count and `--parity` |
| `MINIO_SITE_NAME` | not needed; there is no external metadata service |
| `MINIO_PROMETHEUS_AUTH_TYPE bearer` | `--metrics-token` |
| `MINIO_PROMETHEUS_URL` | `http://<console-address>/metrics` |

## The one behavior to watch

The full list is in the [compatibility matrix](/reference/compatibility/), but in
practice one difference bites: `CreateBucket` is strict. liteio returns `409` for
a bucket you already own, where MinIO returns `200` for `us-east-1`. If your
infrastructure-as-code calls `create-bucket` and expects it to be idempotent,
either tolerate `BucketAlreadyOwnedByYou` or switch to a `head-bucket` existence
check before creating.
