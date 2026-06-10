---
title: "Installation"
description: "Build liteio from source or download a pre-built binary."
weight: 20
---

## Requirements

- Go 1.26 or later.
- Any POSIX OS. Linux is the primary target. macOS works for development.

## Build from source

```bash
git clone https://github.com/tamnd/liteio
cd liteio
make build
```

This builds the `liteio` binary in the repo root. `make install` copies it to
`$GOBIN` or `$GOPATH/bin`.

To cross-compile for Linux from macOS:

```bash
GOOS=linux GOARCH=amd64 go build -o liteio-linux-amd64 ./cmd/liteio/
```

## Download a pre-built binary

Pre-built binaries are attached to each GitHub release:

```bash
# Replace X.Y.Z with the latest release tag.
curl -L https://github.com/tamnd/liteio/releases/download/vX.Y.Z/liteio-linux-amd64 \
     -o /usr/local/bin/liteio
chmod +x /usr/local/bin/liteio
```

## Verify the build

```bash
liteio --version
```

The output includes the version, commit hash, and Go toolchain version.

## Run the tests

```bash
make test         # unit and integration suite
make bench        # every benchmark once (smoke pass)
```
