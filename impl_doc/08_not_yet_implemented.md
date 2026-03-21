# Remaining Unimplemented and Incomplete Features in gookv

## 1. Overview

This document tracks features in the gookv codebase that are not yet fully implemented or remain partially complete. Items are verified against the Go source code.

As of 2026-03-20, two rounds of feature completion have been performed, followed by a cross-region transactional client implementation. The first round (branch `feat/remaining-items-and-multiregion-e2e`) addressed 12 items, and the second round (branch `feature/lack-features-3`, commit `c52b215b7`) implemented 6 additional features. The features implemented in the first round were:

- **Async Commit / 1PC gRPC path** — `KvPrewrite` handler routes to 1PC or async commit paths based on request flags.
- **KvCheckSecondaryLocks** — Full handler with lock inspection and commit detection.
- **KvScanLock** — Iterates CF_LOCK with version filter and limit.
- **CLI `compact`** — Already implemented with `CompactAll()`/`CompactCF()` and `--flush-only` flag.
- **CLI `dump` SST parsing** — `--sst` flag for direct SST file reading via `pebble/sstable`.
- **Raw KV partial RPCs** — `RawBatchScan`, `RawGetKeyTTL` (with full TTL encoding), `RawCompareAndSwap` (TTL-aware), `RawChecksum` (CRC64 XOR).
- **TSO integration** — Server uses PD-allocated timestamps for 1PC commitTS and async commit maxCommitTS.
- **PD leader failover / retry** — Endpoint rotation, exponential backoff, reconnection on failure.
- **PD-coordinated split** — Split check ticker in peer, SplitCheckWorker wired in coordinator, AskBatchSplit/ExecBatchSplit/ReportBatchSplit flow.
- **Engine traits conformance tests** — 17 test cases covering WriteBatch, Snapshot, Iterator, cross-CF, concurrency.
- **Codec fuzz tests** — 6 fuzz targets for bytes and number codecs using `testing.F`.

Additionally, the **Client Library for Multi-Region Routing** has been implemented in `pkg/client/`:

- **PDStoreResolver** — Dynamic `storeID → gRPC address` resolution via PD, with TTL caching.
- **RegionCache** — Client-side `key → region → leader store` cache with sorted-slice binary search and error-driven invalidation.
- **RegionRequestSender** — gRPC connection pool with automatic retry on region errors (NotLeader, EpochNotMatch, etc.).
- **RawKVClient** — Full Raw KV API: Get, Put, PutWithTTL, Delete, GetKeyTTL, BatchGet, BatchPut, BatchDelete, Scan (cross-region), DeleteRange, CompareAndSwap, Checksum.
- **Server-side region validation** — `validateRegionContext()` added to 8 read-only Raw KV handlers for proper `region_error` propagation.
- **9 E2E tests** validating region routing, batch operations, scan across regions, and CAS.

The second round (`feature/lack-features-3`) implemented the following 6 features:

- **Snapshot transfer (end-to-end)** — `SnapWorker` wired via `SetSnapTaskCh`, `handleReady` applies snapshots via `storage.ApplySnapshot()`, `sendRaftMessage` detects `MsgSnap` and uses `SendSnapshot` streaming, gRPC `Snapshot` handler receives chunks, `HandleSnapshotMessage` attaches data and creates peers, `reportSnapshotStatus` feeds back to Raft.
- **Store goroutine** — `RunStoreWorker` started in `main.go`, `HandleRaftMessage` falls back to `storeCh` on `ErrRegionNotFound` for dynamic peer creation via the store worker.
- **Significant messages** — `handleSignificantMessage` now handles all three types: `Unreachable` (calls `rawNode.ReportUnreachable`), `SnapshotStatus` (calls `rawNode.ReportSnapshot`), and `MergeResult` (sets `stopped = true`).
- **GC safe point PD centralization** — `pdclient.Client` interface extended with `GetGCSafePoint` and `UpdateGCSafePoint` methods (15 methods total), `PDSafePointProvider` wraps the PD client, `KvGC` handler calls `UpdateGCSafePoint` after local GC.
- **KvDeleteRange** — `ModifyTypeDeleteRange` (value 2) and `EndKey` field added to `Modify` struct, `KvDeleteRange` gRPC handler implemented, `raftcmd` serialization supports delete-range operations.
- **PD scheduling** — `Scheduler` struct with `scheduleReplicaRepair` and `scheduleLeaderBalance` strategies, `MetadataStore` tracks store liveness (`storeLastHeartbeat`, `IsStoreAlive`, `GetDeadStores`, `GetLeaderCountPerStore`), `RegionHeartbeat` runs scheduler and returns commands, `PDWorker.handleSchedulingCommand` and `sendScheduleMsg` deliver commands to peers, `Peer.handleScheduleMessage` executes TransferLeader/ChangePeer/Merge.

A third implementation round (branch `cross-region-txn`, commit `616cacf16`) added the **cross-region transactional client (TxnClient)** in `pkg/client/`:

- **TxnKVClient** — Transactional KV API entry point: `Begin()` allocates a start timestamp from PD and returns a `TxnHandle`. Supports functional options (`WithPessimistic`, `WithAsyncCommit`, `With1PC`, `WithLockTTL`).
- **TxnHandle** — Per-transaction handle with `Get()`, `BatchGet()`, `Set()`, `Delete()`, `Commit()`, `Rollback()`. Buffers mutations in a local `membuf` and acquires pessimistic locks eagerly in pessimistic mode.
- **LockResolver** — Resolves stale locks encountered during reads by checking transaction status (`checkTxnStatus`) and committing or rolling back (`resolveLock`). Uses a channel-based `resolving` map for deduplication.
- **twoPhaseCommitter** — Executes the 2PC protocol: `selectPrimary` → prewrite (primary-first, secondaries parallel) → `getCommitTS` from PD → `commitPrimary` (sync) → `commitSecondaries` (background). Supports 1PC and async commit paths.
- **Server-side enhancements** — `LockError` structured error type (in `mvcc` package) replaces bare `ErrKeyIsLocked`, enabling full `LockInfo` propagation in read RPCs via `lockToLockInfo()`. Multi-region Raft proposal routing via `proposeModifiesToRegionsWithRegionError()`. `BatchRollbackModifies()` for cluster-mode rollback.

A subsequent fix (branch `new-demo-impl`, commit `e09c358fe`) resolved 6 infrastructure bugs required for cross-region transactions to work end-to-end, and added a cross-region transaction demo (`make txn-demo-start/verify/stop` with `scripts/txn-demo-verify/main.go`):

- **SplitCheckCfg wiring** — `SplitCheckCfg` and `SplitCheckTickInterval` are now properly passed from TOML config through `main.go` to the coordinator, instead of falling back to hardcoded defaults.
- **PD metadata query for child peers** — `maybeCreatePeerForMessage` queries PD via `GetRegionByID()` for full region metadata when creating child peers after split, falling back to minimal metadata from the Raft message.
- **MVCC codec key decoding** — `groupModifiesByRegion()` now decodes MVCC codec-encoded keys (via `mvcc.DecodeKey`) before region routing, because modify keys use `EncodeLockKey`/`EncodeKey` encoding.
- **Narrowest-match region resolution** — `ResolveRegionForKey` selects the most specific region (largest startKey) among matches, handling stale parent regions after split.
- **Context RegionId in KvPrewrite/KvCommit** — Standard 2PC `KvPrewrite` and `KvCommit` use `req.GetContext().GetRegionId()` directly instead of multi-region grouping, since the client groups mutations by region.
- **Proposal timeout as retriable error** — `proposeErrorToRegionError()` treats timeout errors as `NotLeader`, enabling client retry.

A fifth implementation round (branch `feat/add-node`) added **dynamic node addition** — joining new KVS nodes to a running cluster via PD, with automatic region rebalancing:

| # | Feature | Status |
|---|---------|--------|
| 1 | Server-side PDStoreResolver (`internal/server/pd_resolver.go`) | Done |
| 2 | Join mode startup with store ID persistence (`internal/server/store_ident.go`) | Done |
| 3 | Store state machine — Up/Disconnected/Down/Tombstone (`internal/pd/server.go`) | Done |
| 4 | Region balance scheduler (`internal/pd/scheduler.go:scheduleRegionBalance`) | Done |
| 5 | Excess replica shedding scheduler (`internal/pd/scheduler.go:scheduleExcessReplicaShedding`) | Done |
| 6 | MoveTracker — 3-step region move protocol (`internal/pd/move_tracker.go`) | Done |
| 7 | Snapshot send semaphore — concurrent limit of 3 (`internal/server/coordinator.go`) | Done |
| 8 | gookv-ctl `store list` and `store status` commands (`cmd/gookv-ctl/main.go`) | Done |
| 9 | `GetAllStores` pdclient method (`pkg/pdclient/client.go`) | Done |
| 10 | E2E tests for node addition (`e2e/add_node_test.go`) | Done |

## 2. Remaining Items

| # | Category | Feature | Status | Notes |
|---|----------|---------|--------|-------|
| 1 | gRPC / Coprocessor | BatchCoprocessor | Not implemented | Only `Coprocessor` and `CoprocessorStream` are wired. `BatchCoprocessor` remains a stub. |
| 2 | Client Library | TSO batching | Not implemented | Batch `GetTS` calls and dispense from local buffer. Low priority optimization. |
| 3 | Raftstore | Streaming snapshot generation | Not implemented | Current implementation holds all region data in memory; may OOM for large regions. |
| 4 | Raftstore | Region epoch validation in handleScheduleMessage | Not implemented | Currently relies on Raft's built-in rejection. |
| 5 | PD | Store heartbeat capacity fields | Not implemented | Capacity/Available/UsedSize not yet populated. |

### 2.1 BatchCoprocessor

`BatchCoprocessor` is a server-streaming RPC that dispatches a coprocessor request across multiple regions in a single call. Only `Coprocessor` (unary) and `CoprocessorStream` (server-streaming, single region) are currently implemented. `BatchCoprocessor` falls through to `UnimplementedTikvServer`.

This is a low-priority item since the single-region `Coprocessor` and `CoprocessorStream` RPCs cover the core functionality. `BatchCoprocessor` would primarily be a performance optimization for multi-region queries.
