---
title: "Quick start"
description: "A running S3 endpoint and your first bucket in under two minutes."
weight: 30
---

This page starts liteio in single-node mode and walks through creating a bucket,
uploading an object, and reading it back. All you need is the `liteio` binary and
the AWS CLI (or `mc`).

## Start the server

Pick three or more directories for liteio to use as drives. The directories must
exist; liteio formats them on first run.

```bash
mkdir -p /tmp/liteio/{d1,d2,d3,d4}

liteio \
  --address :9000 \
  --drives /tmp/liteio/d1,/tmp/liteio/d2,/tmp/liteio/d3,/tmp/liteio/d4 \
  --access-key minioadmin \
  --secret-key minioadmin
```

The server prints its listen address and starts accepting S3 requests. The web
console is on port 9001 by default (`--console-address`).

For persistent storage, replace `/tmp/liteio/d{1..4}` with real disk paths. A
parity level of N/2 (the default) means the cluster survives losing half its
drives. Set `--parity` to tune the tradeoff.

## Configure the AWS CLI

Add a named profile so you do not have to pass `--endpoint-url` on every command:

```bash
aws configure --profile liteio
```

Enter `minioadmin` / `minioadmin` for the key and secret, and any region (e.g.
`us-east-1`). Then add the endpoint to `~/.aws/config`:

```ini
[profile liteio]
aws_access_key_id     = minioadmin
aws_secret_access_key = minioadmin
region                = us-east-1
endpoint_url          = http://localhost:9000
```

Now every `aws s3` command uses that profile by default when you pass
`--profile liteio`.

## Create a bucket

```bash
aws s3api create-bucket --bucket photos --profile liteio
```

```json
{
    "Location": "/photos"
}
```

## Upload an object

```bash
echo "hello liteio" > hello.txt
aws s3 cp hello.txt s3://photos/hello.txt --profile liteio
```

```
upload: ./hello.txt to s3://photos/hello.txt
```

## List the bucket

```bash
aws s3 ls s3://photos --profile liteio
```

```
2026-06-10 12:00:00         13 hello.txt
```

## Download and verify

```bash
aws s3 cp s3://photos/hello.txt - --profile liteio
```

```
hello liteio
```

## Use mc instead

If you prefer the MinIO client:

```bash
mc alias set local http://localhost:9000 minioadmin minioadmin

mc mb local/photos
mc cp hello.txt local/photos/
mc ls local/photos/
```

## Open the console

Navigate to `http://localhost:9001` and sign in with `minioadmin` / `minioadmin`.
The console shows the cluster topology, drive health, bucket browser, IAM
management, and Prometheus metrics.

## Next steps

- [Single-node deployment](/deployment/single-node/) — production checklist
  including TLS, process supervision, and log rotation.
- [Distributed cluster](/deployment/distributed/) — run liteio across multiple
  nodes for fault tolerance and horizontal scale.
- [Configuration reference](/getting-started/configuration/) — every flag and
  environment variable.
