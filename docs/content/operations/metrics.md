---
title: "Metrics"
description: "Prometheus metrics exposed by liteio."
weight: 10
---

liteio exposes a Prometheus-compatible metrics endpoint on the console port.
Set `--metrics-token` to enable it; requests must carry the token as a bearer
credential.

```bash
curl -H "Authorization: Bearer scrape-token" \
  http://localhost:9001/metrics
```

## Scrape configuration

```yaml
scrape_configs:
  - job_name: liteio
    bearer_token: scrape-token
    static_configs:
      - targets: [liteio.example.com:9001]
    metrics_path: /metrics
```

## Metric catalog

### Request metrics

| Metric | Type | Description |
|---|---|---|
| `liteio_s3_requests_total` | counter | Total S3 requests, labeled `api`, `bucket` |
| `liteio_s3_errors_total` | counter | S3 errors, labeled `api`, `code` |
| `liteio_s3_request_duration_seconds` | histogram | Request latency, labeled `api` |
| `liteio_s3_inflight_requests` | gauge | Requests currently being processed |
| `liteio_s3_bytes_in_total` | counter | Upload bytes received |
| `liteio_s3_bytes_out_total` | counter | Download bytes sent |

### Cluster health metrics

| Metric | Type | Description |
|---|---|---|
| `liteio_cluster_drives_online` | gauge | Drives currently reachable |
| `liteio_cluster_drives_total` | gauge | Total configured drives |
| `liteio_cluster_sets_degraded` | gauge | Erasure sets with at least one offline drive |
| `liteio_cluster_sets_below_read_quorum` | gauge | Sets that cannot serve reads |
| `liteio_cluster_capacity_raw_bytes` | gauge | Raw storage bytes across all drives |
| `liteio_cluster_capacity_usable_bytes` | gauge | Usable bytes after parity overhead |
| `liteio_cluster_capacity_used_bytes` | gauge | Bytes currently in use |
| `liteio_cluster_heal_queue_depth` | gauge | Objects queued for reactive healing |

### Go runtime metrics

Standard `go_*` and `process_*` metrics from the Go runtime are also exported:
GC pause duration, heap size, goroutine count, CPU usage.

## Example queries

Requests per second by API:

```promql
rate(liteio_s3_requests_total[5m])
```

p99 GET latency:

```promql
histogram_quantile(0.99, rate(liteio_s3_request_duration_seconds_bucket{api="GetObject"}[5m]))
```

Error rate:

```promql
rate(liteio_s3_errors_total[5m]) / rate(liteio_s3_requests_total[5m])
```

Capacity used fraction:

```promql
liteio_cluster_capacity_used_bytes / liteio_cluster_capacity_usable_bytes
```
