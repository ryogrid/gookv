# Prometheus Metrics for gookv — Detailed Design

## 1. Background

gookv's KVS server and PD server already expose a `/metrics` endpoint via `promhttp.Handler()` (registered in `internal/server/status/status.go:50`). However, only Go runtime default metrics (`go_*`, `process_*`) are returned — no application-specific metrics are registered.

TiKV exposes hundreds of Prometheus metrics across its KVS and PD servers. This document defines a subset of metrics appropriate for gookv, following TiKV's naming conventions where applicable.

## 2. TiKV Metrics Survey

### 2.1 Naming Conventions

| Component | TiKV Prefix | gookv Prefix |
|-----------|-------------|--------------|
| KVS gRPC handlers | `tikv_grpc_msg_*` | `gookv_grpc_msg_*` |
| Raftstore | `tikv_raftstore_*` | `gookv_raftstore_*` |
| Storage/scheduler | `tikv_storage_*`, `tikv_scheduler_*` | `gookv_storage_*` |
| PD server | `pd_server_*`, `pd_scheduler_*` | `gookv_pd_*` |

### 2.2 TiKV Metrics Not Applicable to gookv

| TiKV Metric | Reason |
|-------------|--------|
| `tikv_coprocessor_*` | gookv has no coprocessor |
| `tikv_resolved_ts_*` | gookv has no CDC/resolved-ts subsystem |
| `tikv_scheduler_throttle_*` | gookv has no write flow control |
| `tikv_gcworker_*` | gookv GC is minimal (no dedicated worker) |
| `tikv_server_cpu_cores_quota` | Not relevant for single-binary deployment |
| `pd_scheduler_hot_region*` | gookv PD scheduler is simplified |

### 2.3 Histogram Bucket Patterns (from TiKV)

| Use Case | Buckets | Range |
|----------|---------|-------|
| gRPC latency | `ExponentialBuckets(0.00005, 2.0, 22)` | 50µs – 104s |
| Storage command | `ExponentialBuckets(0.00001, 2.0, 26)` | 10µs – 671s |
| PD heartbeat | `ExponentialBuckets(0.0005, 2, 13)` | 500µs – 4s |

For gookv, we use a simplified common bucket set. Note: `prometheus.ExponentialBuckets` returns `[]float64` (no error in the version used):
```go
var defaultDurationBuckets = []float64{
    0.0001, 0.0002, 0.0004, 0.0008, 0.0016, 0.0032, 0.0064, 0.0128,
    0.0256, 0.0512, 0.1024, 0.2048, 0.4096, 0.8192, 1.6384, 3.2768,
    6.5536, 13.1072, 26.2144, 52.4288,
} // 100µs – 52s, factor 2.0, 20 buckets
```

## 3. Metric Definitions

### 3.1 gRPC Request Metrics

**File**: `internal/server/metrics.go` (new)

| Metric | Type | Labels | Description | Instrumentation Point |
|--------|------|--------|-------------|----------------------|
| `gookv_grpc_msg_duration_seconds` | Histogram | `type` | Per-RPC latency | `server.go`: each handler entry/exit |
| `gookv_grpc_msg_total` | Counter | `type` | Per-RPC count | `server.go`: each handler entry |
| `gookv_grpc_msg_fail_total` | Counter | `type` | Failed RPCs | `server.go`: when handler returns error/region error |

**Label `type` values** (fixed enum, low cardinality):
`kv_get`, `kv_scan`, `kv_batch_get`, `kv_prewrite`, `kv_commit`, `kv_batch_rollback`, `kv_check_txn_status`, `kv_pessimistic_lock`, `kv_txn_heartbeat`, `kv_resolve_lock`, `kv_cleanup`, `kv_scan_lock`, `kv_gc`, `kv_delete_range`, `raw_get`, `raw_put`, `raw_delete`, `raw_scan`, `raw_batch_get`, `raw_batch_put`

**Code pattern** (applied to each handler):
```go
func (svc *tikvService) KvGet(ctx context.Context, req *kvrpcpb.GetRequest) (*kvrpcpb.GetResponse, error) {
    start := time.Now()
    defer func() {
        grpcMsgDuration.WithLabelValues("kv_get").Observe(time.Since(start).Seconds())
    }()
    grpcMsgTotal.WithLabelValues("kv_get").Inc()
    // ... existing logic ...
}
```

### 3.2 Raft Consensus Metrics

**File**: `internal/raftstore/metrics.go` (new)

| Metric | Type | Labels | Description | Instrumentation Point |
|--------|------|--------|-------------|----------------------|
| `gookv_raftstore_proposal_total` | Counter | — | Normal write proposals | `peer.go:propose()` (only covers write proposals) |
| `gookv_raftstore_read_index_total` | Counter | — | ReadIndex requests | `peer.go:handleReadIndexRequest()` |
| `gookv_raftstore_admin_cmd_total` | Counter | `type` | Admin commands | `peer.go:applySplitAdminEntry()` (label: `split`), `applyCompactLogEntry()` (`compact_log`), `applyConfChangeEntry()` (`conf_change`) |
| `gookv_raftstore_handle_ready_duration_seconds` | Histogram | — | Raft Ready processing time | `peer.go:handleReady()` entry/exit |
| `gookv_raftstore_apply_duration_seconds` | Histogram | — | Entry apply time | Both paths must be instrumented: (1) `peer.go:applyInline()` — timer at entry/exit; (2) `peer.go:submitToApplyWorker()` captures `start` on `ApplyTask`, `onApplyResult()` observes `time.Since(task.StartTime)`. This covers both sync and async apply modes. |
| `gookv_raftstore_log_persist_duration_seconds` | Histogram | — | Raft log persist time (SaveReady/BuildWriteTask) | `peer.go:handleReady()` around SaveReady/BuildWriteTask |
| `gookv_raftstore_send_message_total` | Counter | — | Raft messages sent | `peer.go:handleReady()` after sending messages |
| `gookv_raftstore_leader_count` | Gauge | — | Regions where this peer is leader | Updated in `handleReady()` on leader change |
| `gookv_raftstore_region_count` | Gauge | — | Total region count on this store | Increment in `coordinator.CreatePeer()` / `BootstrapRegion()`, decrement in `DestroyPeer()` |
| `gookv_raftstore_snapshot_send_total` | Counter | — | Snapshots sent | `coordinator.go:HandleSnapshotMessage()` |

### 3.3 Storage Layer Metrics

**File**: `internal/server/metrics.go` (same file as gRPC metrics)

| Metric | Type | Labels | Description | Instrumentation Point |
|--------|------|--------|-------------|----------------------|
| `gookv_storage_command_duration_seconds` | Histogram | `type` | Storage command latency | `storage.go`: `Get`, `Scan`, `BatchGet`, `Prewrite*`, `Commit*`, `ApplyModifies` |
| `gookv_storage_apply_batch_size` | Histogram | — | Modify batch size | `storage.go:ApplyModifies()` — count of modifies |

**Label `type` values**: `get`, `scan`, `batch_get`, `prewrite`, `commit`, `rollback`, `apply_modifies`

### 3.4 ReadIndex & Propose Metrics

**File**: `internal/server/metrics.go`

| Metric | Type | Labels | Description | Instrumentation Point |
|--------|------|--------|-------------|----------------------|
| `gookv_readindex_duration_seconds` | Histogram | — | ReadIndex round-trip time | `coordinator.go:ReadIndex()` entry/exit |
| `gookv_readindex_batch_size` | Histogram | — | Waiters per batch | `readindex_batcher.go:flushLocked()` — `len(batch)` |
| `gookv_propose_duration_seconds` | Histogram | — | Raft propose round-trip time | `coordinator.go:ProposeModifies()` entry/exit |

### 3.5 PD Server Metrics

**File**: `internal/pd/metrics.go` (new)

| Metric | Type | Labels | Description | Instrumentation Point |
|--------|------|--------|-------------|----------------------|
| `gookv_pd_tso_handle_duration_seconds` | Histogram | — | TSO allocation latency | `pd/server.go:Tso()` per-request timing |
| `gookv_pd_tso_total` | Counter | — | TSO requests handled | `pd/server.go:Tso()` |
| `gookv_pd_region_heartbeat_duration_seconds` | Histogram | — | Region heartbeat processing time | `pd/server.go:RegionHeartbeat()` per-heartbeat |
| `gookv_pd_region_heartbeat_total` | Counter | — | Region heartbeats received | `pd/server.go:RegionHeartbeat()` |
| `gookv_pd_store_heartbeat_total` | Counter | — | Store heartbeats received | `pd/server.go:StoreHeartbeat()` |
| `gookv_pd_region_count` | Gauge | — | Total regions in cluster | Set to `len(s.meta.GetAllRegions())` after processing each `RegionHeartbeat`. If `MetadataStore` does not expose a count accessor, add one or use `len(s.meta.regions)` under lock. |
| `gookv_pd_store_count` | Gauge | — | Total stores in cluster | Set after `StoreHeartbeat` processing. Derive from `MetadataStore`'s store map size. |
| `gookv_pd_schedule_command_total` | Counter | `type` | Scheduler commands issued | `pd/server.go:RegionHeartbeat()` after `scheduler.Schedule()` |

**Label `type` values for schedule commands**: `add_peer`, `remove_peer`, `transfer_leader`

## 4. Registration Pattern

### 4.1 Package-level variables with `init()` registration

Each metrics file defines package-level `var` blocks and registers in `init()`:

```go
// internal/server/metrics.go
package server

import (
    "github.com/prometheus/client_golang/prometheus"
)

var (
    grpcMsgDuration = prometheus.NewHistogramVec(
        prometheus.HistogramOpts{
            Namespace: "gookv",
            Subsystem: "grpc",
            Name:      "msg_duration_seconds",
            Help:      "Duration of gRPC messages.",
            Buckets:   defaultDurationBuckets,
        },
        []string{"type"},
    )
    grpcMsgTotal = prometheus.NewCounterVec(
        prometheus.CounterOpts{
            Namespace: "gookv",
            Subsystem: "grpc",
            Name:      "msg_total",
            Help:      "Total number of gRPC messages.",
        },
        []string{"type"},
    )
    // ... more metrics
)

func init() {
    prometheus.MustRegister(grpcMsgDuration)
    prometheus.MustRegister(grpcMsgTotal)
    // ... more registrations
}
```

### 4.2 Why package-level vars (not per-component structs)

- **Simplicity**: No need to thread metrics objects through constructors
- **Consistency with TiKV**: TiKV uses `lazy_static!` globals (Rust equivalent)
- **Prometheus convention**: The Go Prometheus client library is designed for global registration
- **No hot-path allocation**: `WithLabelValues()` returns a cached observer — safe for hot paths

### 4.3 Label cardinality discipline

- **NO `region_id` label** — unbounded cardinality, would create per-region time series
- **NO `store_id` label** on KVS metrics — each server is a single store
- `type` labels use fixed enums (≤ 20 values)
- PD `type` labels for schedule commands ≤ 5 values

## 5. Performance Considerations

- `time.Now()` + `time.Since()` is ~25ns on Linux — negligible vs Raft round-trip (~20ms)
- `WithLabelValues()` does a map lookup (~50ns) — acceptable
- Histogram `Observe()` involves atomic operations (~10ns) — negligible
- Total overhead per RPC: ~100ns — unmeasurable compared to gRPC + Raft latency
- Avoid calling `Describe()` in hot paths (only called during `/metrics` scrape)

## 6. Files Summary

| File | Status | Content |
|------|--------|---------|
| `internal/server/metrics.go` | **New** | gRPC, storage, ReadIndex, propose metrics |
| `internal/raftstore/metrics.go` | **New** | Raft consensus, leader/region gauges |
| `internal/pd/metrics.go` | **New** | PD TSO, heartbeat, scheduler metrics |
| `internal/server/server.go` | **Modify** | Add timing to each gRPC handler |
| `internal/server/coordinator.go` | **Modify** | Add timing to ReadIndex, ProposeModifies |
| `internal/server/storage.go` | **Modify** | Add timing to Get, Scan, ApplyModifies, etc. |
| `internal/server/readindex_batcher.go` | **Modify** | Add batch size metric |
| `internal/raftstore/peer.go` | **Modify** | Add timing to handleReady, applyInline; update gauges |
| `internal/pd/server.go` | **Modify** | Add timing to Tso, RegionHeartbeat, StoreHeartbeat |
