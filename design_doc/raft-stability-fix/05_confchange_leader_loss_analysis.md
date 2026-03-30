# Analysis: ConfChange Balance Move Causes Permanent Leader Loss

## 1. Problem Statement

After bootstrapping a 5-node cluster with 3 voters (stores 1,2,3) and 2 join nodes (stores 4,5), PD's `scheduleRegionBalance` moves regions to the join stores. This process uses a multi-step ConfChange sequence (AddPeer → TransferLeader → RemovePeer). After the move completes, the affected region permanently loses its leader — no peer can be elected, and client requests fail with "not leader" indefinitely.

## 2. Evidence from Server Logs

**Source**: `/tmp/TestFuzzCluster4294217515/` (5 nodes, stores 1-5, files 002-006)

### 2.1. Region 1 ConfChange Sequence

From store 1's log (002):
```
16:25:30  1 switched to configuration voters=(1 2 3 1000)    ← AddPeer: node 1000 (store 4) added
16:25:35  1 switched to configuration voters=(2 3 1000)      ← RemovePeer: node 1 (store 1) removed
```

After 16:25:35, **region 1 has NO further Raft election activity in any log**. The Raft group is stuck.

### 2.2. Store 4 Never Initialized Region 1

Store 4's log (005) shows activity for **other** regions (1000, 1002, etc.) created by splits, but **zero activity for region 1**. Store 4 never created a peer for region 1 despite being added as voter node 1000.

### 2.3. No Snapshot Transfers

Searching all 5 log files for "snapshot": **zero results**. No MsgSnap was ever sent or received for any region. This confirms store 4 never received the data it needed to participate as a voter for region 1.

### 2.4. Other Regions Healthy

All split-created regions (IDs 1000-1013+) show healthy election sequences with proper leader election. Only region 1 (the one subjected to a balance move) is broken.

## 3. Root Cause Analysis

### 3.1. The Sequence of Failure

```
T0: Region 1 voters = {1, 2, 3}, leader = store 3
T1: PD scheduleRegionBalance → StartMove(region=1, source=store1, target=store4)
T2: PD sends AddPeer(node=1000, store=4) to leader (store 3)
T3: Leader proposes ConfChange AddNode(1000) to Raft
T4: Raft commits AddPeer → voters = {1, 2, 3, 1000}
    Leader updates region metadata → region.Peers includes store 4
    Leader sends heartbeat to PD with updated peer list
T5: PD MoveTracker.Advance() sees hasPeerOnStore(region, store4) = true
    → Advances to MoveStateRemoving
    → Returns RemovePeer(node=1, store=1) command
T6: PD sends RemovePeer to leader via heartbeat response
T7: Leader proposes ConfChange RemoveNode(1) to Raft
T8: Raft commits RemovePeer → voters = {2, 3, 1000}
    Store 1 marks itself as stopped

PROBLEM: Between T4 and T8, the leader should have sent MsgSnap to node 1000.
But store 4:
- Never received a Raft message (no logs)
- Never created a peer for region 1 via maybeCreatePeerForMessage
- Node 1000 is a phantom voter — exists in config but not in reality
```

### 3.2. Why No Snapshot Was Sent

After ConfChange AddNode(1000) is committed at T4:
1. etcd/raft adds node 1000 to the Progress tracker in `ProgressStateProbe` mode
2. etcd/raft should generate `MsgApp` or `MsgSnap` in the next `Ready()` cycle
3. The leader's `sendFunc` calls `sendRaftMessage(regionID, peer.Region(), peerID, msg)`
4. `sendRaftMessage` resolves `msg.To = 1000` → looks up store ID from `region.Peers`
5. Finds store 4 → resolves store 4's address from PD
6. Sends via gRPC

**Potential failure points:**
- Store 4 may not have registered with PD yet (address unknown) → message silently dropped
- Store 4's gRPC listener may not be ready → connection refused
- The message reaches store 4 but `maybeCreatePeerForMessage` fails to create the peer
- The message contains stale region metadata that doesn't match PD's state

### 3.3. MoveTracker Advances Too Early

**File**: `internal/pd/move_tracker.go:88-91`

```go
// MoveStateAdding:
if !hasPeerOnStore(region, move.TargetStoreID) {
    return nil // peer not yet added, wait
}
```

`hasPeerOnStore` checks PD's **region metadata** (from the leader's heartbeat). It confirms the peer is in the region's peer list — NOT that the peer actually exists as a running Raft node on the target store. This is a metadata-only check.

**The race**: PD sees the peer in metadata (from the committed ConfChange) and immediately advances to RemovePeer, before the target store has created the peer and received a snapshot.

### 3.4. Why Region 1 Can't Recover

After RemovePeer removes store 1 (node 1):
- Voters = {2, 3, 1000}
- Quorum = 2 of 3
- Nodes 2 and 3 are both available → should be able to elect

But the logs show **no election attempts**. This suggests:
- etcd/raft with `CheckQuorum=true` may prevent elections when a voter is unreachable
- Or the peer on stores 2/3 is not receiving tick events (stopped running?)
- Or the ConfChange entries in the Raft log prevent progress until node 1000 acknowledges

**Most likely**: etcd/raft's `CheckQuorum` causes the leader to step down when node 1000 doesn't respond, and subsequent elections fail because pre-vote/vote messages to node 1000 time out, even though 2 of 3 nodes are sufficient for quorum.

## 4. Summary

| Issue | Description |
|-------|-------------|
| **Primary cause** | MoveTracker advances from AddPeer to RemovePeer based on metadata, not actual peer readiness |
| **Secondary cause** | No snapshot verification — target store never receives data |
| **Tertiary cause** | After ConfChange, Raft group includes phantom voter → elections fail |
| **Impact** | Permanent leader loss for affected region; all client operations fail |
