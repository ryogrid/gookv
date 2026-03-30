# Design: Fix ConfChange Balance Move Leader Loss

## 1. Fix Strategy

The MoveTracker advances from `MoveStateAdding` to `MoveStateRemoving` as soon as the target peer appears in PD's region metadata. This is too early — the target peer may not yet exist on the target store (no snapshot received). The fix: **add a waiting period between AddPeer and RemovePeer** to allow the new peer to receive a snapshot and join the Raft group.

### Approach: Heartbeat-Based Cooldown

After detecting the new peer in metadata (`MoveStateAdding` → peer found), transition to a new `MoveStateStabilizing` state that waits for N additional heartbeat cycles before advancing to TransferLeader/RemovePeer.

## 2. Changes

### 2.1. `internal/pd/move_tracker.go` — Full Replacement of `Advance()` State Machine

**Add named constant and new state:**

```go
// stabilizeHeartbeats is the number of heartbeat cycles to wait after AddPeer
// before advancing to RemovePeer. This gives the new peer time to receive a
// snapshot and join the Raft group.
const stabilizeHeartbeats = 3

const (
    MoveStateAdding       MoveState = iota
    MoveStateStabilizing              // NEW: waiting for new peer to stabilize
    MoveStateTransferring
    MoveStateRemoving
)
```

**Add `StabilizeCount` to `PendingMove`:**

```go
type PendingMove struct {
    RegionID        uint64
    SourcePeer      *metapb.Peer
    TargetStoreID   uint64
    TargetPeerID    uint64
    State           MoveState
    StartedAt       time.Time
    StabilizeCount  int          // heartbeat cycles in MoveStateStabilizing
}
```

**Add nil-region guard at top of `Advance()`:**

```go
func (t *MoveTracker) Advance(regionID uint64, region *metapb.Region, leader *metapb.Peer) *ScheduleCommand {
    if region == nil {
        return nil
    }
    // ... rest of function
```

**REPLACE the entire `MoveStateAdding` case (lines 88-132).** The old code that directly transitions to Transferring/Removing is DELETED:

```go
case MoveStateAdding:
    if !hasPeerOnStore(region, move.TargetStoreID) {
        return nil
    }
    // Record the target peer ID.
    for _, p := range region.GetPeers() {
        if p.GetStoreId() == move.TargetStoreID {
            move.TargetPeerID = p.GetId()
            break
        }
    }
    // Transition to stabilizing — wait for new peer to receive snapshot
    // and join the Raft group before removing the source peer.
    move.State = MoveStateStabilizing
    move.StabilizeCount = 0
    return nil
```

**ADD new `MoveStateStabilizing` case** (between Adding and Transferring):

```go
case MoveStateStabilizing:
    move.StabilizeCount++
    if move.StabilizeCount < stabilizeHeartbeats {
        return nil // keep waiting
    }
    // Stabilization complete — proceed to transfer/remove.
    sourceStoreID := move.SourcePeer.GetStoreId()
    if leader != nil && leader.GetStoreId() == sourceStoreID {
        move.State = MoveStateTransferring
        transferTarget := pickTransferTarget(region, sourceStoreID, 0)
        if transferTarget == nil {
            move.State = MoveStateRemoving
            return &ScheduleCommand{
                RegionID: regionID,
                ChangePeer: &pdpb.ChangePeer{
                    Peer:       move.SourcePeer,
                    ChangeType: eraftpb.ConfChangeType_RemoveNode,
                },
            }
        }
        return &ScheduleCommand{
            RegionID: regionID,
            TransferLeader: &pdpb.TransferLeader{
                Peer: transferTarget,
            },
        }
    }
    move.State = MoveStateRemoving
    return &ScheduleCommand{
        RegionID: regionID,
        ChangePeer: &pdpb.ChangePeer{
            Peer:       move.SourcePeer,
            ChangeType: eraftpb.ConfChangeType_RemoveNode,
        },
    }
```

The `MoveStateTransferring` and `MoveStateRemoving` cases remain unchanged.

### 2.2. Interaction with `scheduleExcessReplicaShedding`

During `MoveStateStabilizing`, the region temporarily has 4 peers (3 original + 1 newly added). The scheduler's `scheduleExcessReplicaShedding` runs at priority 1, after `moveTracker.Advance` (priority 0). Since `Advance()` returns nil during stabilization, control falls through to excess-shedding.

**Safety**: `scheduleExcessReplicaShedding` already checks `HasPendingMove(regionID)` implicitly via the `Schedule()` priority chain. However, it does NOT currently check. We should add a guard:

In `scheduler.go`, the `Schedule()` function (lines 60-84):

```go
// After moveTracker.Advance (priority 0), if a move is pending for this
// region, skip other schedulers to avoid conflicting ConfChanges.
if s.moveTracker != nil && s.moveTracker.HasPendingMove(regionID) {
    return cmd // cmd is nil if Advance returned nil (stabilizing)
}
```

This ensures that while a move is in the stabilizing phase, `scheduleExcessReplicaShedding` does NOT issue a competing RemovePeer.

### 2.3. `MoveState.String()` Method

Add for debugging:

```go
func (s MoveState) String() string {
    switch s {
    case MoveStateAdding:
        return "Adding"
    case MoveStateStabilizing:
        return "Stabilizing"
    case MoveStateTransferring:
        return "Transferring"
    case MoveStateRemoving:
        return "Removing"
    default:
        return "Unknown"
    }
}
```

## 3. Why `stabilizeHeartbeats = 3`

After ConfChange AddPeer is committed on the leader:

1. **Cycle 1**: Leader's `Ready()` generates `MsgSnap` for the new peer. `sendFunc` sends it to the target store via gRPC. Target store receives it and calls `maybeCreatePeerForMessage` → `CreatePeer`.
2. **Cycle 2**: New peer applies snapshot, starts participating in Raft consensus.
3. **Cycle 3**: Safety margin — ensures the new peer is fully caught up and stable.

| Scenario | PdHeartbeatInterval | Stabilize Wait | Note |
|----------|--------------------|-----------------| -----|
| Test (fuzz) | 5s | 15s | Sufficient for small test regions |
| Production (default) | 60s | 180s | Sufficient for regions up to ~100MB |
| Large regions (>100MB) | 60s | 180s | May be insufficient — see §5 |

### Limitation for Large Regions

For production deployments with regions approaching `RegionMaxSize` (144 MiB default), snapshot transfer may exceed 3 heartbeat cycles (180s). In such cases, the `stabilizeHeartbeats` constant should be increased or replaced with the RPC-based readiness check (§5). This is acceptable for the current implementation because the fuzz test uses small regions (~128KB split size).

## 4. Impact on Existing Behavior

- **3-node clusters**: `MaxPeerCount=3 = NumNodes` → no balance moves → no impact
- **5-node clusters with MaxPeerCount=3**: Balance moves wait 3 heartbeats before RemovePeer → slower but correct
- **Existing unit tests**: `TestMoveTracker_FullCycle` and `TestMoveTracker_SourceNotLeader` must be updated to drive through the Stabilizing state (3 extra `Advance` calls) before asserting commands. See test plan.

## 5. Alternative Considered: Verify Peer Readiness via RPC

Instead of a fixed heartbeat count, query the target store whether the peer is ready (applied snapshot, caught up). More precise but requires new gRPC endpoint, PD client changes, and error handling. Deferred as future optimization for large-region deployments.
