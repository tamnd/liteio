---
title: "Metrics"
description: "The Prometheus metrics liteio exposes."
weight: 10
---

liteio serves Prometheus metrics on the console port. The endpoint is off until
you set `--metrics-token`, and once it is on, a scraper has to present that token
as a bearer credential. No token means no endpoint, so you cannot leak metrics
by forgetting to lock them down.

```bash
curl -H "Authorization: Bearer scrape-token" http://localhost:9001/metrics
```

## Scrape config

```yaml
scrape_configs:
  - job_name: liteio
    bearer_token: scrape-token
    static_configs:
      - targets: [liteio.example.com:9001]
    metrics_path: /metrics
```

## Request metrics

| Metric | Type | Meaning |
|---|---|---|
| `liteio_s3_requests_total` | counter | S3 requests, labeled `api`, `bucket` |
| `liteio_s3_errors_total` | counter | Errors, labeled `api`, `code` |
| `liteio_s3_request_duration_seconds` | histogram | Latency, labeled `api` |
| `liteio_s3_inflight_requests` | gauge | Requests in flight right now |
| `liteio_s3_bytes_in_total` | counter | Upload bytes received |
| `liteio_s3_bytes_out_total` | counter | Download bytes sent |

## Cluster health metrics

| Metric | Type | Meaning |
|---|---|---|
| `liteio_cluster_drives_online` | gauge | Drives reachable now |
| `liteio_cluster_drives_total` | gauge | Drives configured |
| `liteio_cluster_sets_degraded` | gauge | Sets with a drive offline |
| `liteio_cluster_sets_below_read_quorum` | gauge | Sets that cannot serve reads |
| `liteio_cluster_capacity_raw_bytes` | gauge | Raw bytes across all drives |
| `liteio_cluster_capacity_usable_bytes` | gauge | Usable bytes after parity |
| `liteio_cluster_capacity_used_bytes` | gauge | Bytes in use |
| `liteio_cluster_heal_queue_depth` | gauge | Objects waiting to be healed |

The standard `go_*` and `process_*` runtime metrics are exported too: GC pauses,
heap size, goroutine count, and CPU.

## Queries worth keeping

Request rate per API:

```promql
rate(liteio_s3_requests_total[5m])
```

p99 GET latency:

```promql
histogram_quantile(0.99, rate(liteio_s3_request_duration_seconds_bucket{api="GetObject"}[5m]))
```

Error ratio, the number to alert on:

```promql
rate(liteio_s3_errors_total[5m]) / rate(liteio_s3_requests_total[5m])
```

How full you are:

```promql
liteio_cluster_capacity_used_bytes / liteio_cluster_capacity_usable_bytes
```

A non-zero `liteio_cluster_sets_below_read_quorum` is a page-someone event: a set
in that state is returning `503` to clients.
