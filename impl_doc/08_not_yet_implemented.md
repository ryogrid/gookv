# Remaining Unimplemented and Incomplete Features in gookv

## 1. Overview

This document tracks features in the gookv codebase that are not yet fully implemented or remain partially complete. Items are verified against the Go source code and cross-referenced with `tasks/no-implement.md`.

As of 2026-03-19, the majority of previously listed gaps have been addressed. The 14 features that were fully implemented (Raft Snapshot, Region Split, Raft Log GC, Conf Changes, Region Merge, PD Heartbeat Loop, TxnScheduler, MVCC Scanner, Pessimistic Lock / ResolveLock / TxnHeartBeat RPCs, WriteBatch.RollbackToSavePoint, CLI `region` command, Coprocessor gRPC integration, GC Worker, Raw KV API) have been removed from this list. The remaining 12 items fall into four categories: gRPC integration gaps, CLI gaps, PD/coordination gaps, and test coverage gaps.

## 2. Summary Table

| # | Category | Feature | Status | Relevant Files |
|---|----------|---------|--------|----------------|
| 1 | gRPC / Txn | Async Commit / 1PC path integration | Partial | `internal/storage/txn/async_commit.go`, `internal/server/server.go` |
| 2 | gRPC / Txn | KvCheckSecondaryLocks endpoint | Not implemented | `internal/server/server.go` |
| 3 | gRPC / Txn | KvScanLock endpoint | Not implemented | `internal/server/server.go` |
| 4 | gRPC / Coprocessor | BatchCoprocessor | Not implemented | `internal/server/server.go` |
| 5 | CLI | `compact` command (actual compaction) | Partial | `cmd/gookv-ctl/main.go` |
| 6 | CLI | `dump` SST file parsing | Partial | `cmd/gookv-ctl/main.go` |
| 7 | gRPC / Raw KV | Raw KV partial RPCs | Not implemented | `internal/server/server.go` |
| 8 | PD | TSO integration | Partial | `internal/pd/server.go`, `pkg/pdclient/client.go` |
| 9 | PD | PD leader failover / retry | Not implemented | `pkg/pdclient/client.go` |
| 10 | Raftstore / PD | PD-coordinated split in raftstore | Partial | `internal/raftstore/split/checker.go`, `internal/raftstore/peer.go` |
| 11 | Engine | Engine traits test coverage | Partial | `internal/engine/traits/traits_test.go` |
| 12 | Codec | Codec fuzz tests | Not implemented | `pkg/codec/` |

## 3. Details

### 3.1 Async Commit / 1PC gRPC Path Integration

`PrewriteAsyncCommit()` and `Is1PCEligible()` are implemented in `internal/storage/txn/async_commit.go`, but the `KvPrewrite` handler in `internal/server/server.go` does not inspect the `use_async_commit` or `try_one_pc` request flags to select the appropriate execution path. All prewrite requests go through the standard two-phase path.

### 3.2 KvCheckSecondaryLocks Endpoint

The `KvCheckSecondaryLocks` RPC is an unimplemented stub in `internal/server/server.go`. Calls fall through to the embedded `UnimplementedTikvServer`.

### 3.3 KvScanLock Endpoint

The `KvScanLock` RPC is an unimplemented stub in `internal/server/server.go`. Calls fall through to the embedded `UnimplementedTikvServer`.

### 3.4 BatchCoprocessor

Only `Coprocessor` and `CoprocessorStream` RPCs are wired. `BatchCoprocessor` is not implemented and falls through to the embedded `UnimplementedTikvServer`.

### 3.5 CLI `compact` Command

`cmdCompact()` in `cmd/gookv-ctl/main.go` calls `eng.SyncWAL()`, which maps to Pebble's `Flush()`. This flushes the WAL/memtable but does not trigger actual range compaction via Pebble's `Compact()` API.

### 3.6 CLI `dump` SST File Parsing

`cmdDump()` iterates raw key-value pairs from a column family using a standard engine iterator with optional `--decode` for MVCC key decoding. It does not support direct SST/sstable file parsing or SST-specific metadata inspection.

### 3.7 Raw KV Partial RPCs

`RawBatchScan`, `RawGetKeyTTL`, `RawCompareAndSwap`, and `RawChecksum` remain unimplemented stubs in `internal/server/server.go`. The core Raw KV operations (RawGet, RawPut, RawDelete, RawScan, RawBatchGet, RawBatchPut, RawBatchDelete, RawDeleteRange) are implemented in `internal/server/raw_storage.go`.

### 3.8 TSO Integration

The PD server implements the `Tso` RPC and the PD client can call `GetTS`, but the server does not allocate timestamps itself. It relies on client-provided timestamps rather than acting as a centralized timestamp oracle.

### 3.9 PD Leader Failover / Retry

The PD client (`pkg/pdclient/client.go`) connects to the first available endpoint in the provided list. It does not retry failed requests, refresh the PD leader address on connection loss, or implement any failover logic.

### 3.10 PD-Coordinated Split in Raftstore

`SplitCheckWorker` exists in `internal/raftstore/split/checker.go`, but the peer event loop in `internal/raftstore/peer.go` does not handle `PeerTickSplitRegionCheck` to trigger PD-coordinated split proposals. Split execution works when triggered manually, but the automatic size-based trigger through PD is not wired.

### 3.11 Engine Traits Test Coverage

Only minimal tests exist in `internal/engine/traits/traits_test.go`. Substantive TiKV engine_traits tests (WriteBatch atomicity, Snapshot isolation, Iterator boundary conditions) have not been ported.

### 3.12 Codec Fuzz Tests

No fuzz tests using Go's `testing.F` framework exist in `pkg/codec/`. A `TestEncodeDecodeRoundTrip()` test with randomized inputs exists but is not a proper fuzz test.
