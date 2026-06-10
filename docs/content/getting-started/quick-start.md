---
title: "Quick start"
description: "A live S3 endpoint and your first bucket in two minutes."
weight: 30
---

This gets you a running server, a bucket, and an object round-trip. You need the
`liteio` binary (see [installation](/getting-started/installation/)) and the AWS
CLI. If you prefer `mc`, there is a version of every step at the bottom.

## Start the server

Pick four directories for liteio to use as drives. They must exist; liteio
formats them on first run.

```bash
mkdir -p /tmp/liteio/d{1,2,3,4}

liteio \
  --address :9000 \
  --drives /tmp/liteio/d1,/tmp/liteio/d2,/tmp/liteio/d3,/tmp/liteio/d4 \
  --access-key minioadmin \
  --secret-key minioadmin
```

The server starts on port 9000 for S3 and 9001 for the web console. Leave it
running and open a second terminal.

> Using `/tmp` is fine for a first look, but it does not survive a reboot. For
> anything you want to keep, point `--drives` at real disks, one directory per
> physical drive.

## Point the AWS CLI at it

Add a profile so you do not repeat `--endpoint-url` on every command. Put this
in `~/.aws/config`:

```ini
[profile liteio]
aws_access_key_id     = minioadmin
aws_secret_access_key = minioadmin
region                = us-east-1
endpoint_url          = http://localhost:9000
```

## Create a bucket

```bash
aws s3api create-bucket --bucket photos --profile liteio
```

```json
{
    "Location": "/photos"
}
```

## Round-trip an object

```bash
echo "hello liteio" > hello.txt
aws s3 cp hello.txt s3://photos/hello.txt --profile liteio
aws s3 ls s3://photos --profile liteio
aws s3 cp s3://photos/hello.txt - --profile liteio
```

You should see the upload confirmation, a listing line, and `hello liteio`
printed back. That is the full write-list-read path working against erasure-coded
storage.

## Open the console

Go to `http://localhost:9001` and sign in with `minioadmin` / `minioadmin`. The
console shows cluster topology, drive health, a bucket browser, IAM, and live
metrics. The secret key stays server-side; the browser never holds it.

## The same thing with mc

```bash
mc alias set local http://localhost:9000 minioadmin minioadmin
mc mb local/photos
mc cp hello.txt local/photos/
mc ls local/photos/
mc cat local/photos/hello.txt
```

## Where to go next

- [Single node](/deployment/single-node/): the production checklist, TLS,
  systemd, and a reverse proxy.
- [Distributed cluster](/deployment/distributed/): run liteio across machines
  for fault tolerance and scale.
- [S3 API](/s3-api/): the full set of operations with worked examples.
- [Configuration](/getting-started/configuration/): every flag and environment
  variable.
