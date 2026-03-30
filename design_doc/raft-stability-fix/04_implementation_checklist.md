# Implementation Checklist: Bootstrap with min(NumNodes, MaxPeerCount) Voters

Complete each step in order. Each step includes a verification command.

## Phase 1: KVS Server Changes

### Step 1: Add `sort` import and `--max-peer-count` flag

**File**: `cmd/gookv-server/main.go`

Add `"sort"` to the import block. Add flag near other flag definitions (around line 41):

```go
maxPeerCount := flag.Int("max-peer-count", 3,
    "Maximum replicas per region (must match PD --max-peer-count)")
```

**Verify**: `go vet ./cmd/gookv-server/...`

### Step 2: Extract `bootstrapStoreIDs` helper function

**File**: `cmd/gookv-server/main.go`

Add near `parseInitialCluster` (around line 464):

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

**Verify**: `go vet ./cmd/gookv-server/...`

### Step 3: Fix PD bootstrap call (lines 202-213)

**File**: `cmd/gookv-server/main.go`

Replace the peer list construction inside the `if !bootstrapped` block (lines 206-209):

**Before**:
```go
peers := make([]*metapb.Peer, 0, len(clusterMap))
for sid := range clusterMap {
    peers = append(peers, &metapb.Peer{Id: sid, StoreId: sid})
}
```

**After**:
```go
bootstrapSIDs, _ := bootstrapStoreIDs(clusterMap, *maxPeerCount)
peers := make([]*metapb.Peer, 0, len(bootstrapSIDs))
for _, sid := range bootstrapSIDs {
    peers = append(peers, &metapb.Peer{Id: sid, StoreId: sid})
}
```

**Verify**: `go vet ./cmd/gookv-server/...`

### Step 4: Fix Raft bootstrap block (lines 282-296)

**File**: `cmd/gookv-server/main.go`

Replace the entire `if recovered == 0` block with the code from `02_design.md` §2.4. Key changes:
- Call `bootstrapStoreIDs(clusterMap, *maxPeerCount)` to get the subset
- Check if current `*storeID` is in the bootstrap subset
- Bootstrap nodes: call `coord.BootstrapRegion` with subset peers
- Non-bootstrap nodes: log and skip (they start empty, will receive regions from PD via `maybeCreatePeerForMessage` → `CreatePeer` when the leader sends messages)

**Verify**: `go vet ./cmd/gookv-server/...` && `make -f Makefile build`

### Step 5: Add unit test for `bootstrapStoreIDs`

**File**: `cmd/gookv-server/main_test.go` (create if not exists)

Add table-driven test per `03_test_plan.md` §1.1. Test cases: 3/3, 5/3, 7/3, 3/5, 1/3, 5/0.

**Verify**: `go test ./cmd/gookv-server/... -v -run TestBootstrapStoreIDs`

## Phase 2: e2elib Changes

### Step 6: Update `cluster.go` — Split bootstrap and join nodes

**File**: `pkg/e2elib/cluster.go`

In `Start()`:
1. Compute `bootstrapCount = min(NumNodes, maxPeerCount)` where `maxPeerCount` comes from `c.cfg.PDConfig.MaxPeerCount` (default 3)
2. Build `initialCluster` with only first `bootstrapCount` nodes
3. Nodes beyond `bootstrapCount` get `InitialCluster = ""`
4. Use `ExtraFlags` to pass `--max-peer-count` to each KVS node

Also:
5. Add `PdHeartbeatInterval string` field to `GokvClusterConfig`
6. Include `pd-heartbeat-tick-interval` in the TOML config builder

See `02_design.md` §2.5 for complete code.

### Step 6b: Update fuzz test to set `PdHeartbeatInterval`

**File**: `e2e_external/fuzz_cluster_test.go`

In the `GokvClusterConfig`, add:
```go
PdHeartbeatInterval: "5s",  // accelerate PD balance for tests
```

This ensures PD's region balance completes within the topology stabilization timeout.

### Step 7: Remove `MaxPeerCount = NumNodes` workaround

**File**: `pkg/e2elib/cluster.go`

Delete lines 62-64:
```go
// DELETE these lines:
if pdCfg.MaxPeerCount == 0 {
    pdCfg.MaxPeerCount = c.cfg.NumNodes
}
```

**Verify**: `go vet ./pkg/e2elib/...`

## Phase 3: Build and Test

### Step 8: Build all binaries

```bash
make -f Makefile build
```

All 4 binaries must compile.

### Step 9: Run unit tests

```bash
make -f Makefile test
```

All tests must pass.

### Step 10: Run e2e tests

```bash
make -f Makefile test-e2e-external
```

Existing e2e tests (3-node clusters) must pass unchanged.

### Step 11: Run fuzz test with 5 nodes

```bash
FUZZ_ITERATIONS=50 FUZZ_CLIENTS=4 make -f Makefile test-fuzz-cluster
```

The fuzz test uses `fuzzNodeCount=5`. With the workaround removed, PD default `MaxPeerCount=3` applies. 3 bootstrap nodes + 2 join nodes. Must pass.

## Phase 4: Commit

### Step 12: Commit code changes

```
feat: bootstrap initial region with min(NumNodes, MaxPeerCount) voters

Bootstrap the initial Raft region with only the first min(NumNodes,
MaxPeerCount) stores as voters. Remaining nodes start empty and receive
regions via PD scheduling (AddPeer ConfChange + snapshot).

This eliminates ConfChange churn when MaxPeerCount < NumNodes:
- No excess replicas to remove at startup
- Stable leadership from first election
- MaxPeerCount=3 (default) works for any cluster size

Changes:
- gookv-server: add --max-peer-count flag, extract bootstrapStoreIDs(),
  fix both PD bootstrap and Raft bootstrap to use voter subset
- e2elib/cluster.go: split bootstrap/join nodes, pass --max-peer-count
  via ExtraFlags, remove MaxPeerCount=NumNodes workaround

Co-Authored-By: Claude Opus 4.6 (1M context) <noreply@anthropic.com>
```

## Phase 5: Documentation Update

### Step 13: Update documentation

**File**: `gookv-design/10_not_yet_implemented.md`
- Mark item #11 as **Resolved**
- Update description to reference the fix

**File**: `current_impl/05_pd.md`
- Update the `--max-peer-count` constraint note to say the constraint is resolved
- Document the new bootstrap behavior

**File**: `USAGE.md`
- Add `--max-peer-count` to the gookv-server CLI flags table

### Step 14: Commit documentation

```
docs: mark MaxPeerCount voter count constraint as resolved

Bootstrap now uses min(NumNodes, MaxPeerCount) voters. The constraint
that --max-peer-count must equal node count is no longer needed.
```
