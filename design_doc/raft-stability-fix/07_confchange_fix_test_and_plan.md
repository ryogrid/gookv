# Test Plan and Implementation Plan: ConfChange Balance Move Fix

## 1. Unit Tests

### 1.1. New: `TestMoveTracker_StabilizingWait`

**File**: `internal/pd/move_tracker_test.go`

- Start a move, simulate AddPeer appearing in region metadata
- Call Advance (from Adding) → transitions to Stabilizing, returns nil
- Call Advance 3 more times (Stabilizing state):
  - Call 1: StabilizeCount=1 < 3 → returns nil
  - Call 2: StabilizeCount=2 < 3 → returns nil
  - Call 3: StabilizeCount=3 ≥ 3 → transitions to Removing, returns RemoveNode command
- Total: 4 Advance calls from the point the peer appears (1 Adding + 3 Stabilizing)
- Source is NOT leader → goes directly to Removing

### 1.2. New: `TestMoveTracker_StabilizingWithLeaderOnSource`

Same setup, but leader is on the source store. After stabilization, should return TransferLeader (not RemoveNode).

### 1.3. New: `TestMoveTracker_NilRegionDuringStabilizing`

- Start a move, advance to Stabilizing
- Call Advance with `region = nil`
- Should return nil without panicking

### 1.4. New: `TestMoveTracker_StaleCleanupDuringStabilizing`

- Start a move, advance to Stabilizing
- Call CleanupStale with a timeout less than elapsed time
- Verify move is cleaned up

### 1.5. Update Existing Tests

**`TestMoveTracker_FullCycle`**: After AddPeer appears in metadata, insert 3 extra Advance calls (returning nil) before asserting the TransferLeader command.

**`TestMoveTracker_SourceNotLeader`**: After AddPeer appears, insert 3 extra Advance calls before asserting RemoveNode.

**All other tests that call Advance with target peer present**: Update to account for 3-heartbeat stabilization.

### 1.6. New: `TestSchedule_PendingMoveBlocksExcessShedding`

**File**: `internal/pd/scheduler_test.go`

- Set up: region with 4 peers (post-AddPeer), maxPeerCount=3
- A pending move exists for this region (in MoveStateStabilizing)
- Call Schedule() → should return nil (not RemoveNode from excess shedding)
- Verifies that HasPendingMove blocks excess shedding during stabilization

## 2. E2E / Fuzz Test

### 2.1. Fuzz Test: 5 Nodes, Default MaxPeerCount=3

**File**: `e2e_external/fuzz_cluster_test.go`

- `fuzzNodeCount=5`
- Do NOT set `PDConfig.MaxPeerCount` → defaults to 3
- `PdHeartbeatInterval: "5s"` to accelerate balance
- The stabilizing wait prevents leader loss
- Seeding, initial audit, chaos phase, final audit must pass

```bash
FUZZ_ITERATIONS=50 FUZZ_CLIENTS=4 make -f Makefile test-fuzz-cluster
```

### 2.2. Existing E2E Tests

All existing tests (3-node clusters) must pass unchanged:

```bash
make -f Makefile test-e2e-external
```

## 3. Implementation Plan

### Step 1: Modify `move_tracker.go`

1. Add `const stabilizeHeartbeats = 3`
2. Add `MoveStateStabilizing` to state enum (between Adding and Transferring)
3. Add `StabilizeCount int` to `PendingMove`
4. Add nil-region guard at top of `Advance()`
5. **REPLACE** the entire `MoveStateAdding` case — old exit logic (lines 100-132) is deleted
6. **ADD** `MoveStateStabilizing` case
7. Add `MoveState.String()` method

**Verify**: `go vet ./internal/pd/...`

### Step 2: Modify `scheduler.go` — Block excess shedding during pending moves

In `Schedule()`, after `moveTracker.Advance`, add:

```go
if s.moveTracker != nil && s.moveTracker.HasPendingMove(regionID) {
    return cmd
}
```

**Verify**: `go vet ./internal/pd/...`

### Step 3: Update existing unit tests

**File**: `internal/pd/move_tracker_test.go`

Update `TestMoveTracker_FullCycle`, `TestMoveTracker_SourceNotLeader`, and any other tests that assert immediate TransferLeader/RemoveNode after AddPeer. Insert 3 additional Advance calls in stabilization state.

### Step 4: Add new unit tests

Add tests from §1.1-1.6.

**Verify**: `go test ./internal/pd/... -v`

### Step 5: Remove fuzz test MaxPeerCount workaround

**File**: `e2e_external/fuzz_cluster_test.go`

Ensure `GokvClusterConfig` does NOT set `PDConfig.MaxPeerCount`. PD default (3) should work.

### Step 6: Build and full test

```bash
go vet ./...
make -f Makefile build
make -f Makefile test
make -f Makefile test-e2e-external
FUZZ_ITERATIONS=50 FUZZ_CLIENTS=4 make -f Makefile test-fuzz-cluster
```

### Step 7: Commit

```
fix: add stabilizing wait in MoveTracker before RemovePeer

After AddPeer ConfChange is committed, wait stabilizeHeartbeats (3)
heartbeat cycles in MoveStateStabilizing before advancing to
RemovePeer. This gives the new peer time to receive a snapshot and
join the Raft group, preventing permanent leader loss.

Also block scheduleExcessReplicaShedding while a move is pending,
preventing conflicting ConfChanges during the stabilization window.

Co-Authored-By: Claude Opus 4.6 (1M context) <noreply@anthropic.com>
```

## 4. Review Findings Addressed

| # | Finding | Resolution |
|---|---------|------------|
| 1 | Old MoveStateAdding exit code must be fully replaced | §3 Step 1.5: explicit "REPLACE" instruction |
| 2 | 3 heartbeats may be insufficient for large regions | 06_design.md §3 Limitation documented |
| 3 | No nil-region guard | Step 1.4: add nil guard at top of Advance() |
| 4 | Existing unit tests will break | Step 3: explicit update instructions |
| 5 | scheduleExcessReplicaShedding races with stabilizing | Step 2: HasPendingMove guard in Schedule() |
| 6 | Off-by-one in test description | §1.1: clarified "4 Advance calls total" |
| 7 | No stale cleanup test during stabilizing | §1.4: TestMoveTracker_StaleCleanupDuringStabilizing |
| 8 | Magic number 3 | Step 1.1: const stabilizeHeartbeats = 3 |
| 9 | No MoveState.String() | Step 1.7: String() method added |
