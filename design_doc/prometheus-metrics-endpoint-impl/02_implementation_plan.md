# Prometheus Metrics Implementation Plan

## Phase 1: gRPC Request Metrics (Highest Visibility)

The most immediately useful metrics — answers "how many requests, how fast."

### Files to Create
- [ ] `internal/server/metrics.go` — Define `grpcMsgDuration`, `grpcMsgTotal`, `grpcMsgFailTotal`, `defaultDurationBuckets`, `init()` registration

### Files to Modify
- [ ] `internal/server/server.go` — Add timing instrumentation to each gRPC handler

### Instrumentation Pattern

For each handler in `server.go`, add at the top of the function:

```go
start := time.Now()
defer func() {
    grpcMsgDuration.WithLabelValues("kv_get").Observe(time.Since(start).Seconds())
}()
grpcMsgTotal.WithLabelValues("kv_get").Inc()
```

For error counting, the failure condition varies per handler:
- `KvGet`, `KvScan`, `KvBatchGet`: `resp.RegionError != nil || resp.Error != nil`
- `KvPrewrite`: `resp.RegionError != nil || len(resp.Errors) > 0`
- `KvCommit`, `KvBatchRollback`, `KvCleanup`, `KvCheckTxnStatus`, `KvResolveLock`, `KvTxnHeartBeat`, `KvPessimisticLock`: `resp.RegionError != nil || resp.Error != nil`
- `Raw*` handlers: `resp.RegionError != nil || resp.Error != ""`

Note: `BatchCommands` delegates to individual handlers internally. The per-handler metrics will be incremented for each sub-command — this is intentional (consistent with TiKV). `BatchCommands` itself is NOT listed in the handler table because it does not need separate instrumentation.

### Handlers to Instrument (server.go)

| Handler | Line | Label |
|---------|------|-------|
| `KvGet` | 184 | `kv_get` |
| `KvScan` | 233 | `kv_scan` |
| `KvPrewrite` | 283 | `kv_prewrite` |
| `KvCommit` | 489 | `kv_commit` |
| `KvBatchGet` | 578 | `kv_batch_get` |
| `KvBatchRollback` | 628 | `kv_batch_rollback` |
| `KvCleanup` | 705 | `kv_cleanup` |
| `KvCheckTxnStatus` | 762 | `kv_check_txn_status` |
| `KvPessimisticLock` | 827 | `kv_pessimistic_lock` |
| `KvTxnHeartBeat` | 922 | `kv_txn_heartbeat` |
| `KvResolveLock` | 967 | `kv_resolve_lock` |
| `KvCheckSecondaryLocks` | 1013 | `kv_check_secondary_locks` |
| `KvScanLock` | 1050 | `kv_scan_lock` |
| `KvGC` | 1624 | `kv_gc` |
| `KvDeleteRange` | 1664 | `kv_delete_range` |
| `RawGet` | 1212 | `raw_get` |
| `RawPut` | 1341 | `raw_put` |
| `RawDelete` | 1367 | `raw_delete` |
| `RawScan` | 1392 | `raw_scan` |
| `RawBatchGet` | 1412 | `raw_batch_get` |
| `RawBatchPut` | 1429 | `raw_batch_put` |
| `KVPessimisticRollback` | 872 | `kv_pessimistic_rollback` |

### Verification
```bash
go build ./internal/server/...
go vet ./internal/server/...
# Start a single-node server, send a KvGet, curl /metrics and verify gookv_grpc_msg_duration_seconds appears
```

---

## Phase 2: Raft & Storage Metrics

Answers "where is the latency in the Raft consensus and storage paths."

### Files to Create
- [ ] `internal/raftstore/metrics.go` — Define Raft metrics, `init()` registration. **Important**: This package cannot import `internal/server` (circular dependency). Define its own `defaultDurationBuckets` (same values as `server/metrics.go`).

### Files to Modify
- [ ] `internal/server/metrics.go` — Add `readindexDuration`, `proposeDuration`, `storageCommandDuration`, `readindexBatchSize`, `applyBatchSize`
- [ ] `internal/server/coordinator.go` — Add timing to `ReadIndex()`, `ProposeModifies()`
- [ ] `internal/server/storage.go` — Add timing to `Get`, `Scan`, `BatchGet`, `ApplyModifies`, `PrewriteModifies`, `CommitModifies`
- [ ] `internal/server/readindex_batcher.go` — Add `readindexBatchSize.Observe(float64(len(batch)))` in `flushLocked()`
- [ ] `internal/raftstore/peer.go` — Add timing to `handleReady()`, `applyInline()`; update leader/region gauges

### Instrumentation Details

**coordinator.go — ReadIndex:**
```go
func (sc *StoreCoordinator) ReadIndex(regionID uint64, timeout time.Duration) error {
    start := time.Now()
    defer func() {
        readindexDuration.Observe(time.Since(start).Seconds())
    }()
    // ... existing logic
}
```

**coordinator.go — ProposeModifies:**
```go
func (sc *StoreCoordinator) ProposeModifies(...) error {
    start := time.Now()
    defer func() {
        proposeDuration.Observe(time.Since(start).Seconds())
    }()
    // ... existing logic
}
```

**storage.go — each method:**
```go
func (s *Storage) Scan(...) ([]*KvPairResult, error) {
    start := time.Now()
    defer func() {
        storageCommandDuration.WithLabelValues("scan").Observe(time.Since(start).Seconds())
    }()
    // ... existing logic
}
```

**peer.go — handleReady:**
```go
func (p *Peer) handleReady() {
    start := time.Now()
    defer func() {
        handleReadyDuration.Observe(time.Since(start).Seconds())
    }()
    // ... existing logic
}
```

**peer.go — apply duration** (both sync and async paths):
- `applyInline()`: `start := time.Now()` at entry, `applyDuration.Observe(...)` at exit
- `submitToApplyWorker()`: store `time.Now()` on `ApplyTask.StartTime` field (add field if needed)
- `onApplyResult()`: `applyDuration.Observe(time.Since(task.StartTime).Seconds())`

**peer.go — region count gauge**: Increment `raftstoreRegionCount` in `coordinator.CreatePeer()` and `BootstrapRegion()`, decrement in `DestroyPeer()`.

**peer.go — leader/region gauges** (updated on leader status change in `handleReady`):
```go
if p.isLeader.Load() && !wasLeader {
    raftstoreLeaderCount.Inc()
} else if !p.isLeader.Load() && wasLeader {
    raftstoreLeaderCount.Dec()
}
```

### Verification
```bash
go build ./...
go vet ./...
# Run fuzz test, curl /metrics during execution, verify:
# - gookv_readindex_duration_seconds histogram
# - gookv_raftstore_handle_ready_duration_seconds histogram
# - gookv_raftstore_leader_count gauge
```

---

## Phase 3: PD Metrics

Answers "how is PD performing for TSO and scheduling."

### Files to Create
- [ ] `internal/pd/metrics.go` — Define PD metrics, `init()` registration

### Files to Modify
- [ ] `internal/pd/server.go` — Add timing to `Tso()`, `RegionHeartbeat()`, `StoreHeartbeat()`; update region/store gauges

### Instrumentation Details

**pd/server.go — Tso:**

Note: `Tso()` has a follower-forwarding early return (`s.forwardTso(stream)`). Place metrics INSIDE the main loop body, AFTER the follower check, to avoid counting forwarded streams.

```go
func (s *PDServer) Tso(stream pdpb.PD_TsoServer) error {
    // ... follower forwarding check is BEFORE the loop — metrics go inside
    for {
        // ... recv
        start := time.Now()
        // ... allocate TSO
        pdTsoHandleDuration.Observe(time.Since(start).Seconds())
        pdTsoTotal.Inc()
        // ... send
    }
}
```

**pd/server.go — RegionHeartbeat:**
```go
func (s *PDServer) RegionHeartbeat(stream pdpb.PD_RegionHeartbeatServer) error {
    for {
        // ... recv
        start := time.Now()
        // ... process heartbeat
        pdRegionHeartbeatDuration.Observe(time.Since(start).Seconds())
        pdRegionHeartbeatTotal.Inc()
        // ... send response (if scheduler command)
    }
}
```

### Verification
```bash
go build ./internal/pd/...
go vet ./internal/pd/...
# Start PD with status-addr, curl /metrics, verify gookv_pd_tso_handle_duration_seconds
```

---

## Phase 4: Validation

- [ ] `go vet ./...`
- [ ] `make -f Makefile build`
- [ ] `make -f Makefile test`
- [ ] Run fuzz test: `FUZZ_ITERATIONS=100 FUZZ_CLIENTS=4 go test ./e2e_external/... -v -count=1 -timeout 900s -run TestFuzzCluster`
- [ ] During fuzz test, `curl http://<status-addr>/metrics | grep gookv_` to verify all metric families appear
- [ ] Verify no performance regression (compare fuzz test duration before/after)

---

## Testing Strategy

### Unit Tests

No dedicated unit tests for metrics registration — metrics are tested implicitly via integration:
- If registration fails (duplicate name, invalid label), `prometheus.MustRegister` panics at `init()` time — caught by any test that imports the package.
- If instrumentation is wrong (e.g., wrong label count), `WithLabelValues` panics — caught by handler tests.

### Integration Verification

Add a lightweight check in an existing e2e test or as a manual step:
```bash
# During any running cluster:
curl -s http://127.0.0.1:<status-port>/metrics | grep "gookv_grpc_msg_duration_seconds"
curl -s http://127.0.0.1:<pd-status-port>/metrics | grep "gookv_pd_tso"
```

### Regression Check

Compare fuzz test `txnOK` count and total duration before and after metrics. The overhead should be unmeasurable (< 0.1% of total runtime).
