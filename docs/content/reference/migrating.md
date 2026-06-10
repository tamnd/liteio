---
title: "Migrating from MinIO"
description: "Switch an existing MinIO deployment to liteio."
weight: 30
---

liteio aims for drop-in compatibility with MinIO's S3 API. In most cases,
changing the endpoint URL is all that is required.

## Client migration

Point any S3 client at liteio's endpoint. No SDK changes are needed:

```bash
# Before (MinIO)
aws --endpoint-url http://minio.example.com:9000 s3 ls

# After (liteio)
aws --endpoint-url http://liteio.example.com:9000 s3 ls
```

`mc` alias:

```bash
# Before
mc alias set myminio http://minio.example.com:9000 access secret

# After
mc alias set liteio http://liteio.example.com:9000 access secret
```

## Data migration

Use `mc mirror` to copy data from a running MinIO instance to liteio:

```bash
mc alias set src  http://minio.example.com:9000   minio-access minio-secret
mc alias set dest http://liteio.example.com:9000  liteio-access liteio-secret

mc mirror src/ dest/ --preserve
```

`mc mirror` copies objects incrementally, preserving metadata and ETags. Run it
continuously during the migration window (`--watch` flag) to drain the delta,
then cut over DNS or the load balancer endpoint.

For large datasets, run multiple `mc mirror` instances in parallel with
different bucket prefixes.

## IAM migration

The IAM model is compatible. Export MinIO policies:

```bash
mc admin policy list myminio
mc admin policy info myminio readwrite > readwrite.json
```

Import into liteio:

```bash
mc admin policy create liteio readwrite readwrite.json
```

Export and re-create users:

```bash
mc admin user list myminio
# For each user:
mc admin user add liteio alice alicepw
mc admin policy attach liteio readwrite --user alice
```

## Config differences

| MinIO setting | liteio equivalent |
|---|---|
| `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` | `--access-key` / `--secret-key` |
| `MINIO_VOLUMES` | `--drives` |
| `MINIO_ERASURE_SET_DRIVE_COUNT` | not exposed; computed from total drives and `--parity` |
| `MINIO_SITE_NAME` | not needed; no external metadata service |
| `MINIO_PROMETHEUS_AUTH_TYPE bearer` | `--metrics-token` |
| `MINIO_PROMETHEUS_URL` | `http://<console-address>/metrics` |

## Deliberate behavior differences

See the [compatibility matrix](/reference/compatibility/) for the full list.
The main practical difference is `CreateBucket` idempotency: liteio always
returns `409` for a bucket you already own, where MinIO returns `200` for
`us-east-1`. Infrastructure-as-code tools that rely on idempotent
`create-bucket` should add an `--ignore-error BucketAlreadyOwnedByYou` check
or move to a separate `head-bucket` existence check.
