---
title: "Docker"
description: "Run liteio in a container."
weight: 30
---

liteio ships a minimal Docker image built from a scratch base — just the static
binary and no shell. The image is published to the GitHub Container Registry.

## Pull the image

```bash
docker pull ghcr.io/tamnd/liteio:latest
```

## Single-node container

Mount host directories as drives:

```bash
docker run -d \
  --name liteio \
  -p 9000:9000 \
  -p 9001:9001 \
  -v /mnt/drive1:/data/d1 \
  -v /mnt/drive2:/data/d2 \
  -v /mnt/drive3:/data/d3 \
  -v /mnt/drive4:/data/d4 \
  -e LITEIO_DRIVES=/data/d1,/data/d2,/data/d3,/data/d4 \
  -e LITEIO_ACCESS_KEY=admin \
  -e LITEIO_SECRET_KEY=changeme \
  ghcr.io/tamnd/liteio:latest
```

## Docker Compose

```yaml
services:
  liteio:
    image: ghcr.io/tamnd/liteio:latest
    ports:
      - "9000:9000"
      - "9001:9001"
    volumes:
      - drive1:/data/d1
      - drive2:/data/d2
      - drive3:/data/d3
      - drive4:/data/d4
    environment:
      LITEIO_DRIVES: /data/d1,/data/d2,/data/d3,/data/d4
      LITEIO_ACCESS_KEY: admin
      LITEIO_SECRET_KEY: changeme
    restart: unless-stopped

volumes:
  drive1:
  drive2:
  drive3:
  drive4:
```

## Build the image locally

The Dockerfile is in the repo root:

```bash
docker build -t liteio:local .
```

The build uses a multi-stage Go build and produces a scratch-based image. The
final image is typically under 20 MB.
