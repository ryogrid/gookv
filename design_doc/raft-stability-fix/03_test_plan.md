# Test Plan: Bootstrap with min(NumNodes, MaxPeerCount) Voters

## 1. Unit Tests

### 1.1. `bootstrapStoreIDs` Function

**File**: `cmd/gookv-server/main_test.go` (new file or add to existing)

Test the extracted `bootstrapStoreIDs(clusterMap, maxPeerCount)` function:

| NumNodes | MaxPeerCount | Expected Bootstrap | Expected Join |
|----------|-------------|-------------------|---------------|
| 3 | 3 | {1,2,3} | {} |
| 5 | 3 | {1,2,3} | {4,5} |
| 7 | 3 | {1,2,3} | {4,5,6,7} |
| 3 | 5 | {1,2,3} | {} |
| 1 | 3 | {1} | {} |
| 5 | 0 | {1,2,3,4,5} | {} |

Verify:
- Bootstrap IDs are sorted ascending
- Join IDs are sorted ascending
- Union of bootstrap + join = all store IDs

### 1.2. Existing Unit Tests

Run `make -f Makefile test` and verify all existing tests pass. Key packages:
- `internal/pd/scheduler_test.go` (MaxPeerCount behavior)
- `pkg/e2elib/cli_test.go`

## 2. E2E Tests

### 2.1. 3-Node Cluster — Baseline (No Change in Behavior)

**Test**: Use existing `newClusterWithLeader(t)` — creates 3-node cluster
- All 3 nodes are bootstrap voters (min(3,3) = 3)
- REGION LIST shows 3 peers for region 1
- PUT/GET works

### 2.2. 5-Node Cluster — 3 Voters + 2 Join

**Test**: `TestCluster5NodePartialBootstrap`
- Config: `NumNodes=5`, `PDConfig.MaxPeerCount=3`
- Expected:
  - Nodes 1-3: bootstrap voters for region 1
  - Nodes 4-5: start empty (join mode)
  - REGION LIST shows 3 peers initially
  - STORE LIST shows all 5 stores
  - PD scheduler may add peers on stores 4-5 for region balance
- Verify: PUT/GET works immediately after leader election (30s timeout)

### 2.3. Fuzz Test — 5 Nodes, Default MaxPeerCount=3

**Test**: Existing `TestFuzzCluster` with `fuzzNodeCount=5`
- The `MaxPeerCount=NumNodes` workaround is removed from `cluster.go`
- PD uses default `MaxPeerCount=3`
- 3 bootstrap nodes + 2 join nodes
- Set `PdHeartbeatInterval: "5s"` in `GokvClusterConfig` to accelerate PD balance
- The topology stabilization check (REGION LIST stable for 30s) detects balance completion
- After stabilization: regions distributed across stores, all with 3 peers
- Seeding, initial audit, chaos phase, final audit should all pass

**PD balance timing** (with `pd-heartbeat-tick-interval=5s`):
- Each balance move = 3 heartbeat cycles × 5s = 15s
- With `regionBalanceRateLimit=4`, multiple regions balanced concurrently
- Total balance time: ~15-45s depending on region count
- Topology stabilization check (30s stable) adds 30s → total ~45-75s

```bash
FUZZ_ITERATIONS=50 FUZZ_CLIENTS=4 make -f Makefile test-fuzz-cluster
```

## 3. Regression Tests

### 3.1. Region Split After Partial Bootstrap

- 5-node cluster with `SplitSize=128KB`
- Seed 5000 accounts → triggers splits
- REGION LIST shows split regions with 3 peers each (not 5)
- No ConfChange churn during or after splits

### 3.2. No Excess ConfChange

- 5-node cluster, wait 60s after bootstrap
- Monitor REGION LIST: peer list and leader should stabilize quickly
- `confVer` should NOT keep incrementing
- Contrast with the old behavior (confVer++++ every heartbeat cycle)

### 3.3. Leader Election After Node Stop

- 5-node cluster, stop a bootstrap voter (e.g., node 1)
- Verify PUT/GET still works (2 of 3 voters = quorum)
- Restart node 1 → `RecoverPersistedRegions` recovers its regions
- Verify node 1 rejoins the Raft group

### 3.4. Bootstrap Node Restart

- 5-node cluster, stop node 2 (bootstrap voter)
- Restart node 2
- Verify: `RecoverPersistedRegions` returns > 0, bootstrap skipped
- Verify: `REGION LIST` shows node 2 back in peer list

### 3.5. Join Node Restart

- 5-node cluster, wait for PD to assign regions to node 4
- Stop node 4, restart node 4
- If PD assigned regions: `RecoverPersistedRegions` recovers them
- If PD hasn't assigned yet: node starts empty, waits for assignment

## 4. Negative Tests

### 4.1. Join Node Stopped Before PD Assignment

- 5-node cluster, immediately stop node 5 (before any region assignment)
- Restart node 5
- Verify: starts cleanly with 0 recovered regions
- Verify: PD can assign regions to it later

### 4.2. All Join Nodes Stopped

- 5-node cluster, stop nodes 4 and 5
- Verify: cluster operates normally (3 bootstrap voters = full quorum)
- Restart nodes 4 and 5 → rejoin cleanly

### 4.3. MaxPeerCount Mismatch (Document Only)

- PD: `--max-peer-count=3`, KVS: `--max-peer-count=5`
- KVS bootstraps with 5 voters but PD tries to reduce to 3
- This is a misconfiguration → documented as unsupported, no fix needed

## 5. Test Execution Commands

```bash
# Unit tests (including new bootstrapStoreIDs tests)
go test ./cmd/gookv-server/... -v -run TestBootstrapStoreIDs
make -f Makefile test

# E2E tests
make -f Makefile test-e2e-external

# Fuzz test (5 nodes, MaxPeerCount=3)
FUZZ_ITERATIONS=50 FUZZ_CLIENTS=4 make -f Makefile test-fuzz-cluster
```
