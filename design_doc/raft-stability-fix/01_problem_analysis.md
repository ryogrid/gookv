# Problem Analysis: MaxPeerCount / Initial Voter Count Mismatch

## 1. Summary

gookv bootstraps the initial region (region 1) with **all nodes** in `--initial-cluster` as Raft voters. When the PD scheduler's `MaxPeerCount` (default 3) is less than the number of initial voters, the scheduler immediately begins removing excess peers via ConfChange. This causes continuous ConfChange churn, leadership instability, client routing failures, and potential data loss on restart.

## 2. Current Bootstrap Code Path

### 2.1. Entry Point: `cmd/gookv-server/main.go`

When `--initial-cluster` is specified (cluster mode), the server parses the topology into `clusterMap` at line 158:

```go
clusterMap := parseInitialCluster(*initialCluster)
// Returns map[uint64]string: storeID → address
```

`parseInitialCluster` is defined at lines 464-487.

### 2.2. PD Bootstrap (lines 202-213) — FIRST BUG SITE

Before the Raft-level bootstrap, the server registers the initial region with PD:

```go
if !bootstrapped {
    peers := make([]*metapb.Peer, 0, len(clusterMap))
    for sid := range clusterMap {
        peers = append(peers, &metapb.Peer{Id: sid, StoreId: sid})
    }
    region := &metapb.Region{Id: 1, Peers: peers}
    store := &metapb.Store{Id: *storeID, Address: cfg.Server.Addr}
    _, _ = pdClient.Bootstrap(bsCtx, store, region)
}
```

**Bug**: ALL `clusterMap` entries become peers in the PD-registered region. PD's scheduler uses this peer list and immediately begins ConfChange to reduce to `MaxPeerCount`.

### 2.3. Raft Bootstrap (lines 282-296) — SECOND BUG SITE

```go
peers := make([]*metapb.Peer, 0, len(clusterMap))
raftPeers := make([]raft.Peer, 0, len(clusterMap))
for sid := range clusterMap {
    peers = append(peers, &metapb.Peer{Id: sid, StoreId: sid})
    raftPeers = append(raftPeers, raft.Peer{ID: sid})
}
region := &metapb.Region{Id: 1, Peers: peers}
coord.BootstrapRegion(region, raftPeers)
```

**Bug**: ALL `clusterMap` entries become Raft voters via `rawNode.Bootstrap(allPeers)`.

### 2.4. BootstrapRegion: `internal/server/coordinator.go:204-300`

`BootstrapRegion(region, allPeers)` passes `allPeers` directly to `NewPeer()`.

### 2.5. NewPeer Bootstrap Path: `internal/raftstore/peer.go:187-257`

When `len(peers) > 0`, NewPeer takes the bootstrap path:

```go
if len(peers) > 0 {
    storage.SetApplyState(ApplyState{AppliedIndex: 0, ...})
    storage.SetDummyEntry()
}
// ...
if len(peers) > 0 {
    rawNode.Bootstrap(peers)  // ALL peers become Raft voters
}
```

### 2.6. PD Scheduler: `internal/pd/scheduler.go`

```go
// scheduleExcessReplicaShedding (line 156):
if len(region.GetPeers()) > s.maxPeerCount {
    // Remove peer on the most-loaded store
}
```

With 5 voters and `maxPeerCount=3`, the scheduler issues 2 `RemoveNode` ConfChanges immediately.

## 3. Symptoms

| Symptom | Cause |
|---------|-------|
| `confVer` keeps incrementing | Scheduler issues ConfChange on every heartbeat |
| "not leader for region X" | Leader steps down during ConfChange |
| "epoch not match" | Client's cached region epoch stale |
| "quorum is not active" | ConfChange-added peers can't participate |
| Data loss on restart | Peers not snapshotted before destabilization |

## 4. TiKV's Architecture (Reference)

TiKV bootstraps with a **single voter**:

**File**: `tikv/components/raftstore/src/store/bootstrap.rs:15-24`

```rust
pub fn initial_region(store_id: u64, region_id: u64, peer_id: u64) -> metapb.Region {
    region.mut_peers().push(new_peer(store_id, peer_id));  // SINGLE peer
    region
}
```

PD expands to `max-replicas` (typically 3) via `AddPeer` ConfChange. Additional stores join empty.

**Reference**: `tikv_impl_docs/raft_and_replication.md` section 3.1.

## 5. Proposed Fix

Bootstrap with `min(NumNodes, MaxPeerCount)` voters at **both** sites:
1. PD bootstrap (`pdClient.Bootstrap`) — register region with subset peers
2. Raft bootstrap (`coord.BootstrapRegion`) — create Raft group with subset voters

Non-bootstrap nodes skip both calls and start empty (functionally identical to join mode, but via the `hasInitialCluster` code path).

See `02_design.md` for detailed implementation design.
