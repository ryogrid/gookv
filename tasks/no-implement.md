# Unimplemented Acceptance Criteria

Results of cross-referencing prd-gookv-impl.json acceptanceCriteria against the codebase.

## Not Implemented (6 items)

### IMPL-002: pkg/codec
- **"Fuzz tests for round-trip correctness"**
  - No fuzz tests using Go's `testing.F` exist
  - `TestEncodeDecodeRoundTrip()` uses randomized inputs but is not a proper fuzz test

### IMPL-009: internal/raftstore — Snapshot
- **"Snapshot: SST export and ingest works"**
  - `PeerStorage.Snapshot()` returns an empty snapshot only (storage.go: "In the initial implementation, we return an empty snapshot")
  - No SST export or ingest logic is implemented

### IMPL-009: internal/raftstore — Region Split
- **"Split: region split on size threshold works"**
  - Data structures (`SplitRegionResult`) and constants (`PeerTickSplitRegionCheck`) are defined, but actual split logic (size check, boundary calculation, split execution) is not implemented

### IMPL-011: internal/storage/txn — TxnScheduler
- **"TxnScheduler dispatches commands correctly"**
  - No `TxnScheduler` type or dispatch logic exists
  - Only individual action functions (Prewrite, Commit, Rollback) are implemented

### IMPL-021: cmd/gookv-ctl — Region metadata inspection
- **"Can inspect region metadata"**
  - The `region` command is listed in usage but `cmdRegion()` function is not implemented

### IMPL-021: cmd/gookv-ctl — SST dump
- **"Can dump SST file contents"**
  - `cmdDump()` only scans raw key-value pairs in a column family
  - No SST file parsing or dump functionality is implemented

## Partially Implemented (3 items)

### IMPL-006: internal/engine/traits
- **"Engine trait tests ported from TiKV"**
  - Only 3 trivial tests exist (compile check, error messages, default values)
  - Substantive TiKV engine_traits tests (WriteBatch, Snapshot, Iterator behavior) have not been ported

### IMPL-009: internal/raftstore — Integration tests
- **"Raftstore integration tests pass"**
  - Basic tests exist in peer_test.go (bootstrap, election, proposal)
  - No integration tests for snapshot, split, or multi-region scenarios

### IMPL-021: cmd/gookv-ctl — Manual compaction
- **"Can trigger manual compaction"**
  - `cmdCompact()` only calls `eng.SyncWAL()` (WAL sync)
  - Does not trigger actual RocksDB/Pebble compaction
