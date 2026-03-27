# Design Doc 01: Proposal Callback Reliability

## Issues Covered

| ID | Category | Summary |
|----|----------|---------|
| Raft C5/H5 | Correctness | `pendingProposals` uses `lastIdx+1` which breaks with batched proposals |
| Raft M6 | Correctness | Proposal callback always calls success regardless of entry data |
| Server C5 | Reliability | `ProposeModifies` blocks until timeout when proposal is silently dropped |
| Raft H4 | Correctness | Admin entries (ConfChange, SplitAdmin) are sent to applyFunc alongside data entries |

---

## Problem

### 1. Index-based proposal tracking is unreliable (Raft C5/H5)

**File:** `internal/raftstore/peer.go`, lines 516-524

```go
// Track the proposal callback.
// We use the last index + 1 as the expected index for this proposal.
lastIdx, _ := p.storage.LastIndex()
expectedIdx := lastIdx + 1
if cmd.Callback != nil {
    p.pendingProposals[expectedIdx] = func(_ []byte, _ error) {
        cmd.Callback(nil)
    }
}
```

When multiple proposals are batched before `handleReady()` runs, all of them call
`p.storage.LastIndex()` and get the **same** value. The second proposal overwrites
the first in `pendingProposals` because both compute the same `expectedIdx`. The
first proposal's callback is silently lost.

Even without batching, Raft can reorder or compact entries, so the actual committed
index may differ from `lastIdx+1`. The etcd/raft library does not guarantee that
a locally proposed entry receives exactly `lastIdx+1`.

### 2. Callback always reports success (Raft M6)

**File:** `internal/raftstore/peer.go`, lines 521-523 (propose) and 594-600 (handleReady)

In `propose()`:
```go
p.pendingProposals[expectedIdx] = func(_ []byte, _ error) {
    cmd.Callback(nil)  // Always passes nil (success)
}
```

In `handleReady()`:
```go
for _, e := range rd.CommittedEntries {
    if cb, ok := p.pendingProposals[e.Index]; ok {
        cb(e.Data, nil)  // Also always passes nil error
        delete(p.pendingProposals, e.Index)
    }
}
```

The callback ignores both `e.Data` and the error parameter. If the committed entry
contains data from a **different** proposal (due to leader change or index mismatch),
the original proposer still receives a success callback. The response should verify
that the committed entry actually matches the proposed command.

### 3. Silent proposal drop causes timeout (Server C5)

**File:** `internal/server/coordinator.go`, lines 317-343

```go
doneCh := make(chan struct{}, 1)
cmd := &raftstore.RaftCommand{
    Request: cmdReq,
    Callback: func(resp *raft_cmdpb.RaftCmdResponse) {
        doneCh <- struct{}{}
    },
}
// ...
select {
case <-doneCh:
    return nil
case <-time.After(timeout):
    return fmt.Errorf("raftstore: proposal timeout for region %d", regionID)
}
```

When `rawNode.Propose()` fails (line 508-513 of peer.go), the callback is intentionally
not invoked. This is correct to avoid false success, but it means `ProposeModifies`
always waits the full timeout duration before returning an error to the caller.
There is also no mechanism to clean up stale entries from `pendingProposals` when a
leader steps down or a term changes, causing memory leaks.

### 4. Admin entries leak into applyFunc (Raft H4)

**File:** `internal/raftstore/peer.go`, lines 577-593

```go
for _, e := range rd.CommittedEntries {
    if e.Type == raftpb.EntryConfChange || e.Type == raftpb.EntryConfChangeV2 {
        p.applyConfChangeEntry(e)
    } else if e.Type == raftpb.EntryNormal && IsSplitAdmin(e.Data) {
        eCopy := e
        p.applySplitAdminEntry(&eCopy)
    }
}
// Send to apply worker for state machine application.
if p.applyFunc != nil {
    p.applyFunc(p.regionID, rd.CommittedEntries)
}
```

Admin entries are processed inline and then **also** passed to `applyFunc`. The
`applyEntriesForPeer` function (coordinator.go line 195) attempts to unmarshal them
as `RaftCmdRequest`, which either fails silently or produces garbage modifies.
ConfChange entries have `EntryType == EntryConfChange` and are skipped by the
`entry.Type != raftpb.EntryNormal` check, but SplitAdmin entries **are**
`EntryNormal` with `IsSplitAdmin(data) == true`, so they pass through and are
sent to `req.Unmarshal()` which may partially succeed, producing corrupt modifications.

---

## Current Code

### Peer struct (peer.go lines 100-101)

```go
// pendingProposals tracks in-flight proposals: index -> callback.
pendingProposals map[uint64]func([]byte, error)
```

### propose() (peer.go lines 496-525)

```go
func (p *Peer) propose(cmd *RaftCommand) {
    if cmd.Request == nil {
        slog.Warn("propose: nil request", "region", p.regionID)
        return
    }
    data, err := cmd.Request.Marshal()
    if err != nil {
        slog.Warn("propose: marshal failed", "region", p.regionID, "err", err)
        return
    }
    if err := p.rawNode.Propose(data); err != nil {
        slog.Warn("propose: rawNode.Propose failed", "region", p.regionID, "err", err)
        return
    }
    lastIdx, _ := p.storage.LastIndex()
    expectedIdx := lastIdx + 1
    if cmd.Callback != nil {
        p.pendingProposals[expectedIdx] = func(_ []byte, _ error) {
            cmd.Callback(nil)
        }
    }
}
```

### handleReady() committed entry section (peer.go lines 577-600)

```go
for _, e := range rd.CommittedEntries {
    if e.Type == raftpb.EntryConfChange || e.Type == raftpb.EntryConfChangeV2 {
        p.applyConfChangeEntry(e)
    } else if e.Type == raftpb.EntryNormal && IsSplitAdmin(e.Data) {
        eCopy := e
        p.applySplitAdminEntry(&eCopy)
    }
}
if p.applyFunc != nil {
    p.applyFunc(p.regionID, rd.CommittedEntries)
}
for _, e := range rd.CommittedEntries {
    if cb, ok := p.pendingProposals[e.Index]; ok {
        cb(e.Data, nil)
        delete(p.pendingProposals, e.Index)
    }
}
```

### ProposeModifies (coordinator.go lines 317-343)

```go
doneCh := make(chan struct{}, 1)
cmd := &raftstore.RaftCommand{
    Request:  cmdReq,
    Callback: func(resp *raft_cmdpb.RaftCmdResponse) { doneCh <- struct{}{} },
}
msg := raftstore.PeerMsg{Type: raftstore.PeerMsgTypeRaftCommand, Data: cmd}
if err := sc.router.Send(regionID, msg); err != nil {
    return fmt.Errorf("raftstore: send proposal: %w", err)
}
select {
case <-doneCh:
    return nil
case <-time.After(timeout):
    return fmt.Errorf("raftstore: proposal timeout for region %d", regionID)
}
```

---

## Design

### A. Monotonic proposal ID embedded in entry data

Replace the index-based tracking with a unique proposal ID that is embedded in the
serialized entry data. When the entry commits, extract the ID from the committed data
and match it back to the pending callback.

**Data format change:**

```
[ 8 bytes: proposalID (big-endian uint64) ][ N bytes: original marshaled RaftCmdRequest ]
```

The proposal ID is a per-peer monotonically increasing counter, starting at 1.
Value 0 means "no proposal tracking" (e.g., entries proposed by other leaders).

**Why this works:**
- Each proposal gets a globally unique (within this peer) ID regardless of batching.
- The ID travels through the Raft log, so it is available at commit time.
- No dependence on Raft index assignment timing.

```go
// Peer struct changes:
type Peer struct {
    // ...
    nextProposalID   uint64
    pendingProposals map[uint64]proposalEntry  // proposalID -> entry
}

type proposalEntry struct {
    callback func(*raft_cmdpb.RaftCmdResponse)
    term     uint64
    proposed time.Time
}
```

### B. Immediate error callback on Propose failure

When `rawNode.Propose()` returns an error, invoke the callback immediately with
an error response so `ProposeModifies` unblocks right away instead of waiting
for a timeout.

```go
func (p *Peer) propose(cmd *RaftCommand) {
    if cmd.Request == nil {
        slog.Warn("propose: nil request", "region", p.regionID)
        return
    }

    proposalID := p.nextProposalID
    p.nextProposalID++

    data, err := cmd.Request.Marshal()
    if err != nil {
        slog.Warn("propose: marshal failed", "region", p.regionID, "err", err)
        if cmd.Callback != nil {
            cmd.Callback(errorResponse(err))
        }
        return
    }

    // Prepend proposal ID to data.
    tagged := make([]byte, 8+len(data))
    binary.BigEndian.PutUint64(tagged[:8], proposalID)
    copy(tagged[8:], data)

    if err := p.rawNode.Propose(tagged); err != nil {
        slog.Warn("propose: rawNode.Propose failed", "region", p.regionID, "err", err)
        if cmd.Callback != nil {
            cmd.Callback(errorResponse(err))
        }
        return
    }

    if cmd.Callback != nil {
        p.pendingProposals[proposalID] = proposalEntry{
            callback: cmd.Callback,
            term:     p.rawNode.Status().Term,
            proposed: time.Now(),
        }
    }
}
```

### C. Stale proposal cleanup on term change and periodic sweep

When the leader steps down (detected in `handleReady` via `SoftState`), all pending
proposals from the old term are failed immediately. Additionally, a periodic sweep
(driven by the existing tick mechanism) cleans up proposals older than 2x the
proposal timeout.

```go
// In handleReady(), when leadership is lost:
if !p.isLeader.Load() && wasLeader {
    p.leaseValid.Store(false)
    p.failAllPendingProposals(fmt.Errorf("leader stepped down"))
}

func (p *Peer) failAllPendingProposals(err error) {
    for id, entry := range p.pendingProposals {
        entry.callback(errorResponse(err))
        delete(p.pendingProposals, id)
    }
}

// Periodic sweep in tick handler:
func (p *Peer) sweepStaleProposals(maxAge time.Duration) {
    now := time.Now()
    for id, entry := range p.pendingProposals {
        if now.Sub(entry.proposed) > maxAge {
            entry.callback(errorResponse(fmt.Errorf("proposal expired")))
            delete(p.pendingProposals, id)
        }
    }
}
```

### D. Verify committed entry matches proposal term

When invoking a callback, verify that the committed entry's term matches the
proposal's term. If the term differs, the entry was proposed by a different leader,
and the local proposal should be failed (not falsely reported as success).

```go
for _, e := range rd.CommittedEntries {
    if e.Type != raftpb.EntryNormal || len(e.Data) < 8 {
        continue
    }
    proposalID := binary.BigEndian.Uint64(e.Data[:8])
    if proposalID == 0 {
        continue
    }
    if entry, ok := p.pendingProposals[proposalID]; ok {
        if e.Term == entry.term {
            entry.callback(nil) // success
        } else {
            entry.callback(errorResponse(
                fmt.Errorf("term mismatch: proposed in %d, committed in %d",
                    entry.term, e.Term)))
        }
        delete(p.pendingProposals, proposalID)
    }
}
```

### E. Filter admin entries before applyFunc

Pass only data entries (non-admin) to `applyFunc`. Admin entries have already been
processed inline and must not be re-processed by the apply worker.

```go
// Build filtered list of data-only entries for apply worker.
if p.applyFunc != nil {
    var dataEntries []raftpb.Entry
    for _, e := range rd.CommittedEntries {
        if e.Type == raftpb.EntryNormal && !IsSplitAdmin(e.Data) && len(e.Data) > 8 {
            dataEntries = append(dataEntries, e)
        }
    }
    if len(dataEntries) > 0 {
        p.applyFunc(p.regionID, dataEntries)
    }
}
```

The `applyEntriesForPeer` function in coordinator.go must also be updated to strip
the 8-byte proposal ID prefix before unmarshaling:

```go
func (sc *StoreCoordinator) applyEntriesForPeer(peer *raftstore.Peer, entries []raftpb.Entry) {
    for _, entry := range entries {
        if entry.Type != raftpb.EntryNormal || len(entry.Data) <= 8 {
            continue
        }
        // Strip proposal ID prefix.
        cmdData := entry.Data[8:]
        var req raft_cmdpb.RaftCmdRequest
        if err := req.Unmarshal(cmdData); err != nil {
            continue
        }
        // ... rest of apply logic
    }
}
```

### F. Update ProposeModifies callback to carry errors

The callback type changes from `func(*raft_cmdpb.RaftCmdResponse)` to receive
error information:

```go
// In coordinator.go ProposeModifies:
doneCh := make(chan error, 1)
cmd := &raftstore.RaftCommand{
    Request: cmdReq,
    Callback: func(resp *raft_cmdpb.RaftCmdResponse) {
        if resp != nil && resp.Header != nil && resp.Header.Error != nil {
            doneCh <- fmt.Errorf("raft proposal error: %s", resp.Header.Error.Message)
        } else {
            doneCh <- nil
        }
    },
}
// ...
select {
case err := <-doneCh:
    return err
case <-time.After(timeout):
    return fmt.Errorf("raftstore: proposal timeout for region %d", regionID)
}
```

---

## Implementation Steps

1. **Add proposal ID counter to Peer struct**
   - File: `internal/raftstore/peer.go`
   - Add `nextProposalID uint64` field (initialized to 1 in `NewPeer`)
   - Change `pendingProposals` type from `map[uint64]func([]byte, error)` to `map[uint64]proposalEntry`
   - Add `proposalEntry` struct with `callback`, `term`, `proposed` fields

2. **Add `errorResponse` helper**
   - File: `internal/raftstore/msg.go`
   - Create helper that builds a `RaftCmdResponse` with error header

3. **Rewrite `propose()` method**
   - File: `internal/raftstore/peer.go`, lines 496-525
   - Generate proposal ID, prepend to marshaled data
   - Call callback with error on `rawNode.Propose()` failure
   - Store `proposalEntry` with term and timestamp

4. **Rewrite callback invocation in `handleReady()`**
   - File: `internal/raftstore/peer.go`, lines 577-600
   - Filter admin/split entries out of applyFunc input
   - Match committed entries by proposal ID instead of index
   - Verify term match before invoking success callback

5. **Add stale proposal cleanup**
   - File: `internal/raftstore/peer.go`
   - On leader stepdown: call `failAllPendingProposals()`
   - On tick (e.g., `PeerTickRaft`): periodic sweep of expired proposals

6. **Update `applyEntriesForPeer` to strip proposal ID prefix**
   - File: `internal/server/coordinator.go`, line 195+
   - Skip first 8 bytes when unmarshaling entry data

7. **Update `ProposeModifies` to propagate errors**
   - File: `internal/server/coordinator.go`, lines 317-343
   - Change `doneCh` from `chan struct{}` to `chan error`
   - Parse error from callback response

---

## Test Plan

### Unit Tests

1. **TestProposalIDBatching**
   - Submit 3 proposals in rapid succession before `handleReady()`
   - Verify all 3 callbacks are invoked (not just the last one)
   - Verify each callback receives the correct result

2. **TestProposalFailureCallback**
   - Mock `rawNode.Propose()` to return an error
   - Verify callback is invoked immediately with error (not after timeout)
   - Verify `pendingProposals` map remains empty

3. **TestTermMismatchRejectsCallback**
   - Propose an entry at term T
   - Simulate leader change: committed entry arrives at term T+1
   - Verify the callback receives an error (not false success)

4. **TestStaleProposalCleanup**
   - Add proposals with timestamps in the past
   - Call `sweepStaleProposals()` with a short max age
   - Verify expired proposals get error callbacks
   - Verify non-expired proposals remain

5. **TestLeaderStepdownFailsProposals**
   - Add pending proposals
   - Simulate `SoftState` change to follower
   - Verify all pending proposals get error callbacks

6. **TestAdminEntryFiltering**
   - Create committed entries: [ConfChange, SplitAdmin, Normal]
   - Call `handleReady()`
   - Verify `applyFunc` receives only the Normal entry
   - Verify ConfChange and SplitAdmin are not in the list

7. **TestApplyEntriesStripsProposalID**
   - Create an entry with 8-byte proposal ID prefix + valid RaftCmdRequest
   - Call `applyEntriesForPeer()`
   - Verify the RaftCmdRequest is correctly unmarshaled

### Integration Tests

8. **TestProposeModifiesErrorPropagation**
   - Set up a StoreCoordinator with a peer that rejects proposals
   - Call `ProposeModifies()` and verify it returns an error quickly (< 1s)
   - Verify no timeout wait occurs

9. **TestEndToEndProposalWithSplit**
   - Propose data entries and a split admin entry
   - Verify data entries are applied to KV storage
   - Verify split admin entry is not double-applied
   - Verify all proposal callbacks fire exactly once

---

## Addendum: Review Feedback Incorporated

**Review verdict: NEEDS REVISION -- all blocking issues addressed below.**

### Fix 1: Add `IsCompactLog(data)` check to the entry filter (Section E) -- BLOCKING

The entry filter in Section E missed CompactLog entries. CompactLog entries are `EntryNormal`, start with tag byte `0x01` (not `0x02`, so `IsSplitAdmin` returns false), and are 17 bytes long (passing the `len(e.Data) > 8` check). Without this fix, CompactLog entries would be sent to `applyFunc`, where the 8-byte prefix strip would produce garbage data for protobuf unmarshaling.

**Corrected filter in Section E:**

```go
if p.applyFunc != nil {
    var dataEntries []raftpb.Entry
    for _, e := range rd.CommittedEntries {
        if e.Type == raftpb.EntryNormal && !IsSplitAdmin(e.Data) && !IsCompactLog(e.Data) && len(e.Data) > 8 {
            dataEntries = append(dataEntries, e)
        }
    }
    if len(dataEntries) > 0 {
        p.applyFunc(p.regionID, dataEntries)
    }
}
```

Add `IsCompactLog` helper alongside `IsSplitAdmin`:

```go
// IsCompactLog returns true if the entry data represents a CompactLog admin command.
// CompactLog entries start with tag byte 0x01.
func IsCompactLog(data []byte) bool {
    return len(data) >= 1 && data[0] == 0x01
}
```

Alternatively, a more future-proof approach would be a positive filter that identifies `RaftCmdRequest` entries (which start with protobuf field 1 wire type 2, i.e., `0x0A`), rather than enumerating admin entry types to exclude. This would automatically handle any future admin entry types without filter updates.

### Fix 2: Document non-prefixed entry types (asymmetry in propose paths)

The `propose()` method (peer.go line 496) is the only path that adds the 8-byte proposal ID prefix. The following entries are proposed directly via `p.rawNode.Propose()` and do NOT carry the prefix:

- **CompactLog** at peer.go line 737
- **ReadIndex no-op** at peer.go line 839
- **SplitAdmin** via `ProposeSplit()` at split_admin.go line 168

This is acceptable because these entry types do not use proposal callbacks. The `len(e.Data) < 8` guard in Section D correctly handles these entries by skipping them during callback matching. This guard is not only for "entries proposed by other leaders" but also for all locally-proposed entries that bypass `propose()`.

### Fix 3: Specify `errorResponse()` function body

Sections B, C, and D reference `errorResponse(err)` but the function body was never specified. Implementation Step 2 says to add it to `internal/raftstore/msg.go`. Here is the required implementation:

```go
// errorResponse builds a RaftCmdResponse with an error in the header.
func errorResponse(err error) *raft_cmdpb.RaftCmdResponse {
    return &raft_cmdpb.RaftCmdResponse{
        Header: &raft_cmdpb.RaftResponseHeader{
            Error: &raft_cmdpb.RaftError{
                Message: err.Error(),
            },
        },
    }
}
```

**Design contract**: Success is signaled by `Callback(nil)` (nil `*raft_cmdpb.RaftCmdResponse`). Failure is signaled by `Callback(errorResponse(err))`. Section F's `ProposeModifies` callback correctly checks `resp != nil && resp.Header != nil && resp.Header.Error != nil` to distinguish these cases.

### Fix 4: Backward compatibility for entries without 8-byte prefix during replay

The data format change (prepending 8-byte proposal ID) is not backward-compatible with existing Raft log entries. If a node restarts and replays old entries that lack the prefix, `applyEntriesForPeer` would strip 8 bytes from data that does not have them, corrupting the protobuf payload.

**Mitigation**: Since the Raft log is ephemeral and the applied index is persisted, this only matters during a rolling upgrade scenario. To guard against this:

1. **Require a clean restart** when deploying this change (all nodes stop, then start with new code), OR
2. **Add a prefix-detection guard** in `applyEntriesForPeer`: before stripping the 8-byte prefix, verify that the first 8 bytes decode to a plausible proposal ID (nonzero) and that the remaining bytes successfully unmarshal as `RaftCmdRequest`. If unmarshaling fails after stripping, fall back to unmarshaling the original data without stripping. Example:

```go
func (sc *StoreCoordinator) applyEntriesForPeer(peer *raftstore.Peer, entries []raftpb.Entry) {
    for _, entry := range entries {
        if entry.Type != raftpb.EntryNormal || len(entry.Data) <= 8 {
            continue
        }
        // Try new format (8-byte prefix).
        cmdData := entry.Data[8:]
        var req raft_cmdpb.RaftCmdRequest
        if err := req.Unmarshal(cmdData); err != nil {
            // Fall back to old format (no prefix).
            if err2 := req.Unmarshal(entry.Data); err2 != nil {
                continue
            }
        }
        // ... rest of apply logic
    }
}
```

### Minor: `p.rawNode.Status().Term` performance in `propose()`

`Status()` in etcd/raft v3.5 acquires a lock and copies the entire Raft status. Since `propose()` can be called frequently, consider caching the term from `handleReady()` where `rd.HardState.Term` is already available. Store it as `p.currentTerm` and use that in `propose()` instead of calling `Status()` on each proposal.
