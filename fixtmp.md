# Infrastructure Fixes for Cross-Region Transactions

## 1. Split config not wired from config to coordinator

**Files**: `cmd/gookv-server/main.go`, `internal/config/config.go`

- `SplitCheckCfg` (RegionSplitSize/RegionMaxSize) was never passed from config to the coordinator. The coordinator always fell back to the hardcoded 96MB default.
- Added `SplitCheckTickInterval` field to `RaftStoreConfig` (default 10s) and wired it to `peerCfg`.

## 2. Child region peer creation after split

**File**: `internal/server/coordinator.go`

- `maybeCreatePeerForMessage`: When a Raft message arrives for an unknown region (e.g., a child region after split), the old code created the peer with only 2 peers (from/to from the message). The Raft node couldn't form a quorum because it didn't know the full cluster configuration. Fixed to query PD for full region metadata when available.
- `CreatePeer`: Previously always passed `nil` peers (non-bootstrap). Now checks for persisted Raft state; if none exists, bootstraps with the region's full peer list so the Raft node starts with the correct cluster configuration.
- Added `HasPersistedRaftState` helper in `internal/raftstore/storage.go`.

## 3. MVCC key encoding mismatch in region routing

**File**: `internal/server/server.go`

- `groupModifiesByRegion` compared codec-encoded MVCC keys (from `EncodeLockKey`/`EncodeKey`) against raw region boundaries. This caused all modifies to resolve to wrong regions (typically Region 1). Fixed to decode keys via `mvcc.DecodeKey` before resolving the region.

## 4. Non-deterministic ResolveRegionForKey after split

**File**: `internal/server/coordinator.go`

- After a split, the parent region's metadata on non-split-executor stores retains the original (wide) key range. Multiple regions match the same key, and Go's map iteration order is random, causing `ResolveRegionForKey` to return different regions on each call. Fixed to return the most specific (narrowest) match by selecting the region with the largest startKey among all matches.

## 5. KvPrewrite/KvCommit should use request's region context

**File**: `internal/server/server.go`

- KvPrewrite and KvCommit used `groupModifiesByRegion` to re-group modifies by region. In the TiKV protocol, the client already groups mutations by region before sending — each request targets one specific region (identified in `req.Context.RegionId`). Re-grouping on the server was incorrect and caused propose failures when the store didn't lead all resolved regions. Fixed to use `req.GetContext().GetRegionId()` directly.

## 6. Proposal timeout not handled as region error

**File**: `internal/server/server.go`

- `proposeErrorToRegionError` only handled "not found" and "not leader" errors. A "proposal timeout" was returned as a gRPC Internal error, causing the client to close the connection and retry to the same store indefinitely. Fixed to treat timeout as a NotLeader region error so the client can invalidate its cache and retry with a different store.
