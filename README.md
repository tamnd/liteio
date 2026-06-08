<!-- SPDX-License-Identifier: Apache-2.0 -->
# liteio

A high-performance, S3-compatible object store written in Go.

liteio is a ground-up alternative to MinIO, built for the world that exists after
MinIO's open-source community edition entered maintenance mode in December 2025 and
moved its management features behind the commercial AIStor product. It is
Apache-2.0, ships every management feature for free, and is engineered to saturate
NVMe and the NIC.

> Status: under active development. The package layout, on-disk formats, and
> milestones follow a complete written specification (see [Specification](#specification)).
> The compatibility matrix below tracks exactly what works today.

## Why liteio

liteio optimizes for three things at once, in priority order.

1. **Performance.** Direct I/O on the data path, zero-copy serving, SIMD-accelerated
   Reed-Solomon erasure coding, inline metadata for small objects, and a lean
   inter-node RPC layer. The headline goal is to match or beat MinIO on the WARP
   GET/PUT/STAT/LIST/DELETE mix on identical hardware.
2. **Permissive and complete.** Apache-2.0, no open-core feature gating, and a full
   free web console: bucket management, IAM, policies, lifecycle, tiering, and
   replication. The features MinIO removed from its community edition are
   first-class and free here.
3. **Operational simplicity.** A single static binary, sane defaults, automatic
   healing, and a topology you can reason about. No external database, no metadata
   server, no etcd or ZooKeeper dependency for the core data path.

When a design choice trades raw throughput against a feature or a knob, the default
is throughput and the feature is opt-in.

## What "S3-compatible" means

The external contract is the AWS S3 REST API as spoken by the AWS SDKs, the
`aws s3` / `aws s3api` CLI, `mc`, `rclone`, and the s3fs/boto3 ecosystem. An
unmodified S3 client pointed at a liteio endpoint behaves as it does against AWS S3
for the operations in scope: SigV4 (header, presigned, and streaming `aws-chunked`),
the XML request/response shapes, the error-code catalog, multipart upload
semantics, and the `ETag` / `x-amz-*` header contract.

## Architecture at a glance

liteio uses the three-tier topology that makes deterministic, lookup-free placement
possible:

```
 Cluster
 +- Server Pool 0          a unit of capacity; added to grow
 |  +- Erasure Set 0       a fixed group of N drives; the unit of redundancy
 |  |  +- drive 0 .. N-1   spread across nodes for failure independence
 |  +- Erasure Set 1
 +- Server Pool 1          a later expansion; its own erasure sets
```

An object's location is computed from its name, never looked up:

- **Pool**: a CRC32 over the key, weighted by free space, picks the server pool.
- **Set**: a keyed SipHash-2-4 (salted with the deployment ID) picks the erasure set.
- **Drive order**: a salted permutation rotates which drive holds which shard.

Reads recompute the same three functions and go straight to the drives. There is no
placement database to consult, replicate, or keep consistent.

Two interfaces carry the whole system:

- `ObjectLayer` makes the entire cluster look like one object store to the S3
  handlers (`PutObject`, `GetObject`, `ListObjectsV2`, multipart, ...).
- `StorageAPI` makes every drive look the same whether it is local or one RPC hop
  away. The placement layer never knows which is which, so single-node and
  distributed are the same code.

Data is protected by Reed-Solomon erasure coding over GF(2^8): an object is split
into `K` data shards and `M` parity shards across the set, and any `K` of the `N`
reconstruct it. Every shard carries a HighwayHash checksum verified on read, with
healing on mismatch.

## Quick start

> Requires Go 1.26+.

```sh
git clone https://github.com/tamnd/liteio
cd liteio
make build          # builds ./... ; `make install` puts `liteio` on your PATH
make test           # full unit + integration suite
make bench          # run every benchmark once (smoke)
```

Run a single-node server over a set of drive directories (see `liteio --help`
for the full flag set):

```sh
liteio \
  --address :9000 \
  --drives /mnt/drive1,/mnt/drive2,/mnt/drive3,/mnt/drive4,/mnt/drive5,/mnt/drive6,/mnt/drive7,/mnt/drive8 \
  --parity 4 \
  --access-key liteioadmin --secret-key liteioadmin
```

Point an S3 client at it:

```sh
aws --endpoint-url http://localhost:9000 s3api create-bucket --bucket photos
aws --endpoint-url http://localhost:9000 s3api put-object --bucket photos --key cat.jpg --body cat.jpg
aws --endpoint-url http://localhost:9000 s3 ls s3://photos
```

> The default `aws s3 cp` uploader streams with an `aws-chunked` signature; its
> chunks are decoded and individually signature-verified, so the CLI, presigned
> URLs, `boto3`, and the `s3api` verbs above all work against the current build.

The node also serves the admin REST API and the web console on a second port
(`--console-address`, default `:9001`). Sign in with the same credentials. The
console keeps the secret key server-side and signs admin calls in-process, so the
browser never holds it. Behind plain HTTP for local testing, add
`--console-insecure-cookie`; in production the console must sit behind TLS.

### Running a distributed node

Set `--cluster-address` to run a node in distributed mode. Each node serves the
drives it owns and its lock authority on that address, interprets `--drives` as
endpoint patterns (which may name remote hosts), and takes namespace locks across
a quorum built from `--peers`. A drive endpoint is local to the node when its host
matches `--node-host`.

```sh
liteio \
  --address :9000 \
  --cluster-address :9100 \
  --node-host node1.lan \
  --drives 'https://node{1...4}.lan:9100/mnt/disk{1...8}' \
  --peers https://node2.lan:9100,https://node3.lan:9100,https://node4.lan:9100 \
  --parity 4 \
  --cluster-cert node1.crt --cluster-key node1.key --cluster-ca cluster-ca.crt \
  --cluster-server-name liteio-cluster
```

The same command, with its own `--node-host`, runs on every node. Omit the
`--cluster-*` certificate flags to use plain HTTP on a trusted network.

## Compatibility matrix

Compatibility is tracked honestly, including deliberate divergences. This table is
the living source of truth and is updated as milestones land.

| Area | Status |
| --- | --- |
| Deterministic placement (pool/set/drive) | implemented |
| Reed-Solomon erasure coding + HighwayHash bitrot | implemented |
| `obj.meta` self-describing metadata format | implemented |
| SigV4 header + presigned auth | implemented |
| SigV4 streaming (`aws-chunked`) | implemented |
| Single-part PUT/GET/HEAD/DELETE | implemented |
| Range GET (`bytes=`, suffix, 206/416) | implemented |
| Conditional GET/HEAD (If-Match / If-None-Match / If-\[Un\]Modified-Since) | implemented |
| Bucket lifecycle (Create/Delete/List/Head/Location) | implemented |
| ListObjectsV2 (prefix / delimiter / pagination) | implemented |
| Listing metacache (cached namespace walk) | implemented |
| Batch delete (`DeleteObjects`) | implemented |
| Versioning + delete markers (engine) | implemented |
| Versioning over S3 (`?versioning`) + ListObjectVersions | implemented |
| Multipart upload (Create/Upload/Complete/Abort/List) | implemented |
| CopyObject + UploadPartCopy (`x-amz-copy-source`) | implemented |
| Inter-node RPC transport + remote drive | implemented (M3) |
| Distributed lock service (quorum, leases) | implemented (M3) |
| Namespace locking on mutating operations | implemented (M3) |
| Mutual TLS for inter-node transport | implemented (M3) |
| Cluster topology (endpoint patterns, set layout, format.json) | implemented (M3) |
| Cluster bring-up (format.json lifecycle, drive assembly over RPC) | implemented (M3) |
| Node membership server (serves drives + lock endpoint, mTLS) | implemented (M3) |
| Distributed namespace locking (object layer over a lock quorum) | implemented (M3) |
| Distributed node command (serves drives + lock quorum, mTLS) | implemented (M3) |
| Reactive heal of under-replicated objects + MRF queue | implemented (M3) |
| Cross-node metacache coherence (listings fresh on every node) | implemented (M3) |
| PBAC policy engine (AWS IAM syntax, deny-by-default, canned policies) | implemented (M4) |
| Identity store (users / groups / service accounts, access-key resolution) | implemented (M4) |
| STS temporary credentials (AssumeRole, session intersection, expiry) | implemented (M4) |
| Bucket policies + anonymous/public access (Principal, cross-path combine) | implemented (M4) |
| IAM condition keys (string/bool/IP/numeric/date, Not + IfExists variants) | implemented (M4) |
| Bucket-policy persistence + ?policy GET/PUT/DELETE endpoint | implemented (M4) |
| Front-door authorization (per-request action/ARN/condition mapping, identity + bucket-policy combine) | implemented (M4) |
| Signed admin REST API (IAM CRUD over HTTP, SigV4-authenticated, action-gated) | implemented (M4) |
| STS endpoint (AssumeRole over signed POST form, sessions sign through one authority) | implemented (M4) |
| OIDC web identity federation (AssumeRoleWithWebIdentity, JWT/JWKS, federated sessions) | implemented (M4) |
| Certificate federation (AssumeRoleWithCertificate, X.509 chain validation, subject-to-policy) | implemented (M4) |
| LDAP federation (AssumeRoleWithLDAPIdentity, lookup-then-bind, groups-to-policy) | implemented (M4) |
| Session-token binding (X-Amz-Security-Token validated per request, expiry enforced) | implemented (M4) |
| Free web console (embedded SPA, server-side sessions, signed in-process admin bridge) | in progress (M5) |
| Server runs on the full IAM store: front-door authorization, STS, session validation, admin + console listener | implemented (M5) |
| Admin info/health API + console cluster dashboard (topology, drive reachability, per-set quorum/availability) | implemented (M5) |
| Drive capacity reporting (statfs across local + remote drives, raw + usable bytes per set and deployment) | implemented (M5) |
| Console bucket browser (signed in-process S3 bridge, bucket list/create, object browse) | implemented (M5) |
| Console object actions (upload small objects, download, delete) | implemented (M5) |
| Console identity management (users, policies, attach/detach over the admin API) | implemented (M5) |
| Console groups and service accounts (members, per-user service accounts) | implemented (M5) |
| Lifecycle / encryption / object lock | planned (M6) |
| Replication / tiering / events | planned (M7) |
| Rebalance / decommission | planned (M8) |

### Deliberate divergences

- ACL grants are normalized to the canned ACL set (`private`, `public-read`,
  `public-read-write`, `authenticated-read`); arbitrary grantee grants are
  normalized to the nearest canned policy.
- `CreateBucket` on a bucket you already own returns `BucketAlreadyOwnedByYou`
  (409) in every region, including `us-east-1`, rather than the legacy
  `200 OK` + ACL reset.
- Deferred operations return a correct `NotImplemented` error rather than failing
  opaquely: `SelectObjectContent`, S3 Object Lambda, Access Points,
  Inventory/Analytics/Storage Lens, S3 Batch, Requester Pays, Transfer
  Acceleration.

## Specification

liteio is built from a complete written specification. The design lives in
`Spec 2020` and the as-built implementation notes live alongside it.

- Design spec: vision, S3 contract, data path, placement, coordination,
  performance engine, metadata, IAM, advanced features, operations, the Go
  implementation, and the M0-M9 roadmap.
- Implementation spec: the as-built package layout, on-disk formats, and the test
  and benchmark coverage for each subsystem as it lands.

## Project layout

```
 cmd/liteio/    main: flags, subcommands, wiring
 s3/            S3 REST front door: router, handlers, XML, errors, SigV4
 auth/          IAM: PBAC eval, policy model, users/groups/service accounts, STS
 ldapdir/       LDAP directory client behind the auth Directory seam (federation)
 object/        ObjectLayer: placement, erasure, meta, multipart, lifecycle
 storage/       StorageAPI: local O_DIRECT drive ops, remote RPC drive
 cluster/       topology, format.json, peer membership, locks, inter-node RPC
 heal/          scanner, reactive/proactive heal, MRF queue
 replicate/     bucket replication + tiering
 kms/ crypto/   KMS interface + SSE-S3/KMS/C encryption
 event/         notification targets
 admin/         admin REST API
 console/       embedded web console (go:embed)
 metrics/       Prometheus + OpenTelemetry
 buf/           aligned buffer pools
 pkg/           small shared utilities
```

There are no `internal/` package directories: every package is importable, and
encapsulation is by package boundary and exported-vs-unexported identifiers.

## Contributing

All repository text (README, docs, commit messages, issues, PRs) is in English,
written in a plain human voice. Code must be `gofmt`-clean, pass `go vet`,
`golangci-lint`, and `govulncheck`, and ship with tests. Run `make all` before
opening a PR.

## License

Apache-2.0. See [LICENSE](LICENSE). liteio ships every management feature in the
free binary; that is a deliberate, permanent answer to the open-core squeeze that
made MinIO's community edition unusable.
