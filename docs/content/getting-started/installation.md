---
title: "Installation"
description: "Build liteio from source or download a release binary."
weight: 20
---

## Requirements

- Go 1.26 or newer.
- A POSIX system. Linux is the production target; macOS works for development.

## Build from source

```bash
git clone https://github.com/tamnd/liteio
cd liteio
make build
```

This drops a `liteio` binary in the repo root. `make install` copies it onto
your `PATH` (into `$GOBIN`, or `$GOPATH/bin`).

Cross-compiling a Linux binary from a Mac is the usual one-liner:

```bash
GOOS=linux GOARCH=amd64 go build -o liteio-linux-amd64 ./cmd/liteio/
```

## Download a release binary

Each GitHub release attaches pre-built binaries. Replace `vX.Y.Z` with the tag
you want:

```bash
curl -L https://github.com/tamnd/liteio/releases/download/vX.Y.Z/liteio-linux-amd64 \
     -o /usr/local/bin/liteio
chmod +x /usr/local/bin/liteio
```

## Check it

```bash
liteio --version
```

The output carries the version, the commit it was built from, and the Go
toolchain that built it. Include this in bug reports.

## Run the tests

If you built from source and want to confirm the tree is healthy:

```bash
make test    # unit and integration suite
make bench   # every benchmark once, as a smoke pass
```
