---
title: "Docker"
description: "Run liteio in a container."
weight: 30
---

The image is a `scratch` base with just the static binary and no shell, so it is
small and has almost no attack surface. It is published to the GitHub Container
Registry.

```bash
docker pull ghcr.io/tamnd/liteio:latest
```

## Single node

Mount one host directory per drive and pass the config as environment:

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

## Compose

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

> Use four separate volumes, not four subdirectories of one. liteio treats each
> as an independent drive, and parity only protects you if the drives can fail
> independently.

## Build the image yourself

The `Dockerfile` lives in the repo root. It is a multi-stage build that compiles
the binary and copies it onto `scratch`, producing an image under 20 MB:

```bash
docker build -t liteio:local .
```
