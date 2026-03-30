# Detailed Design: Bootstrap with min(NumNodes, MaxPeerCount) Voters

## 1. Overview

Change the initial region bootstrap to use only the first `min(NumNodes, MaxPeerCount)` stores as Raft voters. Remaining stores start empty — they stay in the `hasInitialCluster` code path but skip `BootstrapRegion` and `pdClient.Bootstrap`. PD distributes regions to them via `scheduleRegionBalance` (multi-step: AddPeer → TransferLeader → RemovePeer).

**Key timing concern**: PD's `scheduleRegionBalance` runs on each RegionHeartbeat. The default `PdHeartbeatTickInterval` is 60 seconds, making balance moves very slow (3 steps × 60s = 180s per region). For tests, set `pd-heartbeat-tick-interval = "5s"` in the TOML config to accelerate balance moves to ~15 seconds total. The existing topology stabilization check (REGION LIST stable for 30s) detects balance completion.

## 2. File-Level Changes

### 2.1. `cmd/gookv-server/main.go` — Add `--max-peer-count` Flag

Add flag near line 41 (with other flag definitions):

```go
import "sort"  // ADD to import block

maxPeerCount := flag.Int("max-peer-count", 3,
    "Maximum replicas per region (must match PD --max-peer-count)")
```

### 2.2. `cmd/gookv-server/main.go` — Extract `bootstrapStoreIDs` Helper

Extract a testable function (add near `parseInitialCluster`):

```go
// bootstrapStoreIDs selects the subset of store IDs that participate in the
// initial Raft group bootstrap. Returns sorted slices of bootstrap and join IDs.
func bootstrapStoreIDs(clusterMap map[uint64]string, maxPeerCount int) (bootstrap, join []uint64) {
    all := make([]uint64, 0, len(clusterMap))
    for sid := range clusterMap {
        all = append(all, sid)
    }
    sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })

    bootstrapCount := len(all)
    if maxPeerCount > 0 && maxPeerCount < bootstrapCount {
        bootstrapCount = maxPeerCount
    }
    return all[:bootstrapCount], all[bootstrapCount:]
}
```

### 2.3. `cmd/gookv-server/main.go` — Fix PD Bootstrap (lines 202-213)

**Current** (all stores):
```go
peers := make([]*metapb.Peer, 0, len(clusterMap))
for sid := range clusterMap {
    peers = append(peers, &metapb.Peer{Id: sid, StoreId: sid})
}
region := &metapb.Region{Id: 1, Peers: peers}
```

**Proposed** (subset only):
```go
bootstrapSIDs, _ := bootstrapStoreIDs(clusterMap, *maxPeerCount)

peers := make([]*metapb.Peer, 0, len(bootstrapSIDs))
for _, sid := range bootstrapSIDs {
    peers = append(peers, &metapb.Peer{Id: sid, StoreId: sid})
}
region := &metapb.Region{Id: 1, Peers: peers}
```

Note: `pdClient.Bootstrap` is called by all nodes concurrently, but PD's `Bootstrap` RPC is idempotent (only the first call succeeds; subsequent calls are no-ops). Since all nodes compute the same deterministic `bootstrapSIDs` (sorted), they will all propose the same region definition. This pre-existing race is safe.

### 2.4. `cmd/gookv-server/main.go` — Fix Raft Bootstrap (lines 282-296)

**Proposed** (replace the entire `if recovered == 0` block):

```go
if recovered == 0 {
    bootstrapSIDs, _ := bootstrapStoreIDs(clusterMap, *maxPeerCount)

    // Check if this node is a bootstrap voter.
    isBootstrapNode := false
    for _, sid := range bootstrapSIDs {
        if sid == *storeID {
            isBootstrapNode = true
            break
        }
    }

    if isBootstrapNode {
        peers := make([]*metapb.Peer, 0, len(bootstrapSIDs))
        raftPeers := make([]raft.Peer, 0, len(bootstrapSIDs))
        for _, sid := range bootstrapSIDs {
            peers = append(peers, &metapb.Peer{Id: sid, StoreId: sid})
            raftPeers = append(raftPeers, raft.Peer{ID: sid})
        }
        region := &metapb.Region{Id: 1, Peers: peers}
        if err := coord.BootstrapRegion(region, raftPeers); err != nil {
            slog.Error("Failed to bootstrap region", "error", err)
            os.Exit(1)
        }
        slog.Info("Raft cluster bootstrapped", "region", 1, "peers", len(raftPeers))
    } else {
        slog.Info("Non-bootstrap node: starting empty, waiting for PD region scheduling",
            "store-id", *storeID, "bootstrap-stores", bootstrapSIDs)
    }
}
```

**Non-bootstrap nodes in `hasInitialCluster` mode**: These nodes remain in the `hasInitialCluster` code path. They have:
- A `PDWorker` running (for store heartbeats)
- A `StoreWorker` running (for `maybeCreatePeerForMessage` — lazy peer creation)
- A `SplitResultHandler` running

When PD's `scheduleReplicaRepair` decides to add a replica on a non-bootstrap store, it issues an `AddPeer` ConfChange. The leader applies it and sends a `MsgSnap` to the new peer. The new store's `HandleRaftMessage` → `maybeCreatePeerForMessage` → `CreatePeer` flow creates the peer and applies the snapshot. This is the same flow as join mode — no special handling needed.

### 2.5. `pkg/e2elib/cluster.go` — Split Bootstrap and Join Nodes

In `Start()`, replace the `initialCluster` construction (around line 87):

```go
// Determine bootstrap subset.
maxPeerCount := c.cfg.PDConfig.MaxPeerCount
if maxPeerCount == 0 {
    maxPeerCount = 3 // PD default
}
bootstrapCount := c.cfg.NumNodes
if maxPeerCount < bootstrapCount {
    bootstrapCount = maxPeerCount
}

// Build initial cluster string for bootstrap nodes only.
var parts []string
for i := 0; i < bootstrapCount; i++ {
    parts = append(parts, fmt.Sprintf("%d=127.0.0.1:%d", i+1, pairs[i].grpc))
}
initialCluster := strings.Join(parts, ",")
```

Update the node creation loop:

```go
for i := 0; i < c.cfg.NumNodes; i++ {
    nodeInitialCluster := initialCluster
    if i >= bootstrapCount {
        nodeInitialCluster = "" // join mode
    }
    nodeCfg := GokvNodeConfig{
        BinaryPath:     c.cfg.ServerBinaryPath,
        StoreID:        uint64(i + 1),
        PDEndpoints:    pdEndpoints,
        InitialCluster: nodeInitialCluster,
        LogLevel:       "info",
        ExtraFlags:     []string{"--max-peer-count", strconv.Itoa(maxPeerCount)},
    }
    // ...
}
```

**Use `ExtraFlags`** (existing field on `GokvNodeConfig`) to pass `--max-peer-count` to KVS nodes. This avoids adding a dedicated struct field.

Also update the TOML config builder to include `pd-heartbeat-tick-interval`:

```go
// Add to GokvClusterConfig:
PdHeartbeatInterval string // e.g., "5s" — accelerates PD region balance

// In the TOML builder:
if c.cfg.PdHeartbeatInterval != "" {
    lines = append(lines, fmt.Sprintf("pd-heartbeat-tick-interval = %q", c.cfg.PdHeartbeatInterval))
}
```

### 2.6. `pkg/e2elib/cluster.go` — Remove `MaxPeerCount = NumNodes` Workaround

Delete lines 62-64:

```go
// DELETE:
// if pdCfg.MaxPeerCount == 0 {
//     pdCfg.MaxPeerCount = c.cfg.NumNodes
// }
```

PD's default `MaxPeerCount=3` now works correctly since only 3 nodes bootstrap as voters.

### 2.7. No Changes Required

These files need **no changes**:
- `cmd/gookv-pd/main.go` — `--max-peer-count` flag already exists
- `internal/pd/server.go` — epoch validation and scheduler logic unchanged
- `internal/server/coordinator.go` — `BootstrapRegion`, `CreatePeer`, `RecoverPersistedRegions`, `sendFunc` closure all remain correct
- `internal/raftstore/peer.go` — bootstrap vs recovery decision unchanged
- `internal/raftstore/storage.go` — `HasPersistedRaftState` unchanged
- `pkg/e2elib/gokvnode.go` — `ExtraFlags` already handled in `Start()` (line 123)

## 3. Impact on Recent Fixes

| Fix | Impact |
|-----|--------|
| `sendFunc` closure (uses `peer.Region()`) | No impact — ConfChanges that ADD peers to non-bootstrap stores work correctly |
| `RecoverPersistedRegions` | No impact — bootstrap nodes persist Raft state normally; non-bootstrap nodes have no state until PD assigns regions |
| `HasPersistedRaftState` (checks both keys) | No impact — behavior unchanged |
| NotLeader leader hint | No impact — leader hints work regardless of initial voter count |
| PD epoch validation in `PutRegion` | No impact — stale heartbeat rejection still works |

## 4. Backward Compatibility

**Existing clusters** (all-voter bootstrap): On restart, `RecoverPersistedRegions` returns > 0 → bootstrap is skipped → peers restored from persisted state. The fix only affects **new cluster initialization** (`recovered == 0`).

**Rolling upgrade**: Not supported. All nodes must be upgraded together.

## 5. Configuration

| Component | Flag | Default | Notes |
|-----------|------|---------|-------|
| gookv-pd | `--max-peer-count` | 3 | Controls PD scheduler target |
| gookv-server | `--max-peer-count` | 3 | Controls bootstrap voter subset |
| e2elib | `PDConfig.MaxPeerCount` | 3 | Auto-propagated to PD and KVS via `ExtraFlags` |

All must agree. Mismatch causes PD to converge toward its own `MaxPeerCount` via ConfChange.
