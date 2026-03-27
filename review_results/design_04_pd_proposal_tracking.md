# Design Doc 04: PD Proposal Tracking, Snapshot Heartbeat, and TSO Overflow

## Issues Covered

| ID | Severity | File | Description |
|----|----------|------|-------------|
| PD C1 | Critical | `internal/pd/raft_peer.go:250-255` | `propose()` uses `LastIndex()+1` which breaks with batched proposals |
| PD C4 | Critical | `internal/pd/snapshot.go:18-29` | `PDSnapshot` omits `storeLastHeartbeat` -- all stores appear Down after snapshot apply |
| PD M1 | Major | `internal/pd/server.go:1194-1215` | TSO logical overflow may produce non-monotonic timestamps |

---

## Issue 1: PD C1 -- Proposal Index Tracking Breaks with Batched Proposals

### Problem

In `internal/pd/raft_peer.go`, the `propose()` method (line 234) tracks pending proposal callbacks by computing an expected log index:

```go
// internal/pd/raft_peer.go:250-255
lastIdx, _ := p.storage.LastIndex()
expectedIdx := lastIdx + 1
if proposal.Callback != nil {
    p.pendingProposals[expectedIdx] = proposal.Callback
}
```

After `rawNode.Propose(data)` is called (line 243), the entry is placed in Raft's **unstable log**, not in `PDRaftStorage`. The storage's `LastIndex()` (defined at `internal/pd/raft_storage.go:132`) only reflects entries that have been persisted via `SaveReady()` in `handleReady()`.

The `Run()` loop (line 192-213) processes **all queued mailbox messages** before calling `handleReady()`:

```go
// internal/pd/raft_peer.go:204-209
case msg, ok := <-p.Mailbox:
    if !ok {
        p.stopped.Store(true)
        return
    }
    p.handleMessage(msg)
```

When multiple proposals arrive in the mailbox between ticks, each call to `propose()` sees the **same** `LastIndex()` value (storage hasn't been updated yet). All proposals compute the same `expectedIdx`, and each overwrites the previous callback in `pendingProposals`. Only the last proposal's callback survives; earlier callbacks are silently lost.

In `handleReady()` (line 312-356), committed entries are matched by `e.Index`:

```go
// internal/pd/raft_peer.go:352-355
if cb, ok := p.pendingProposals[e.Index]; ok {
    cb(result, applyErr)
    delete(p.pendingProposals, e.Index)
}
```

Earlier proposals' entries have valid `e.Index` values but no matching callback, so their `ProposeAndWait` callers hang until context timeout.

### Design

Replace the index-guessing approach with a **monotonic proposal counter** (atomic uint64). The counter is independent of the Raft log index. The proposal's callback is tracked by this counter, and the counter value is embedded in the proposed data so that `handleReady()` can extract it from the committed entry and look up the correct callback.

**Approach**: Embed a proposal ID in the serialized `PDCommand`. Since `PDCommand` is marshaled via `Marshal()` / `UnmarshalPDCommand()`, we wrap the proposal data with a small header containing the proposal ID.

However, a simpler and less invasive approach exists: use a **FIFO queue** instead of a map indexed by log index. Since proposals are applied in the same order they are proposed (Raft guarantees log ordering, and only the leader proposes), we can use a slice/queue of callbacks. Each committed entry that came from this node pops the next callback from the queue.

**Chosen approach**: Replace `pendingProposals map[uint64]func(...)` with `pendingProposals []func([]byte, error)`. On propose, append the callback. On apply of a committed entry originated from this node, dequeue the first callback.

The challenge is detecting which committed entries originated from this node. In etcd/raft, the leader always proposes entries, and a leader's committed entries are always its own proposals (until leadership changes). When leadership changes, all pending callbacks should be drained with `ErrNotLeader`.

**Refined approach**: Use a monotonic counter stored in the `PDRaftPeer` struct. The counter value is **not** embedded in the wire format. Instead, we maintain an ordered slice of `(proposalID, callback)` pairs. Since only the leader proposes, and the leader processes committed entries in order, we simply dequeue callbacks in FIFO order for non-empty, non-conf-change entries. On leader change, drain all pending with `ErrNotLeader`.

### Current Code

```go
// internal/pd/raft_peer.go:84-98
type PDRaftPeer struct {
    // ...
    pendingProposals map[uint64]func([]byte, error)
    // ...
}
```

```go
// internal/pd/raft_peer.go:234-256
func (p *PDRaftPeer) propose(proposal *PDProposal) {
    data, err := proposal.Command.Marshal()
    // ...
    if err := p.rawNode.Propose(data); err != nil {
        // ...
    }
    lastIdx, _ := p.storage.LastIndex()
    expectedIdx := lastIdx + 1
    if proposal.Callback != nil {
        p.pendingProposals[expectedIdx] = proposal.Callback
    }
}
```

### Implementation Steps

1. **Change `pendingProposals` from map to slice** in `PDRaftPeer` struct (`raft_peer.go:98`):

```go
// Before:
pendingProposals map[uint64]func([]byte, error)

// After:
pendingProposals []func([]byte, error)
```

2. **Update `NewPDRaftPeer`** to initialize the slice instead of a map:

```go
// Before:
pendingProposals: make(map[uint64]func([]byte, error)),

// After:
pendingProposals: nil, // zero-value slice is fine
```

3. **Update `propose()`** (`raft_peer.go:234-256`) to append instead of index-based insert:

```go
func (p *PDRaftPeer) propose(proposal *PDProposal) {
    data, err := proposal.Command.Marshal()
    if err != nil {
        if proposal.Callback != nil {
            proposal.Callback(nil, fmt.Errorf("pd: marshal command: %w", err))
        }
        return
    }

    if err := p.rawNode.Propose(data); err != nil {
        if proposal.Callback != nil {
            proposal.Callback(nil, fmt.Errorf("pd: propose: %w", err))
        }
        return
    }

    // Track callback in FIFO order -- dequeued in handleReady().
    if proposal.Callback != nil {
        p.pendingProposals = append(p.pendingProposals, proposal.Callback)
    }
}
```

4. **Update `handleReady()`** (`raft_peer.go:312-356`) to dequeue callbacks in FIFO order:

```go
for _, e := range rd.CommittedEntries {
    if len(e.Data) == 0 {
        // No-op entry (leader election); skip, do NOT dequeue.
        continue
    }
    if e.Type == raftpb.EntryConfChange || e.Type == raftpb.EntryConfChangeV2 {
        // Conf change; skip, do NOT dequeue.
        continue
    }

    cmd, err := UnmarshalPDCommand(e.Data)
    if err != nil {
        slog.Error("pd: failed to unmarshal committed entry",
            "node", p.nodeID, "index", e.Index, "err", err)
        // Dequeue and notify error if this is our proposal.
        if len(p.pendingProposals) > 0 {
            cb := p.pendingProposals[0]
            p.pendingProposals = p.pendingProposals[1:]
            cb(nil, err)
        }
        continue
    }

    var result []byte
    var applyErr error
    if p.applyFunc != nil {
        result, applyErr = p.applyFunc(cmd)
    }

    // Dequeue the next pending callback (FIFO).
    if len(p.pendingProposals) > 0 {
        cb := p.pendingProposals[0]
        p.pendingProposals = p.pendingProposals[1:]
        cb(result, applyErr)
    }
}
```

5. **Drain pending proposals on leader change** in the `handleReady()` SoftState section (`raft_peer.go:268-278`):

```go
if rd.SoftState != nil {
    wasLeader := p.isLeader.Load()
    nowLeader := rd.SoftState.Lead == p.nodeID
    p.isLeader.Store(nowLeader)
    p.leaderID.Store(rd.SoftState.Lead)

    if wasLeader != nowLeader && p.leaderChangeFunc != nil {
        p.leaderChangeFunc(nowLeader)
    }

    // If we lost leadership, drain all pending proposals.
    if wasLeader && !nowLeader {
        for _, cb := range p.pendingProposals {
            cb(nil, ErrNotLeader)
        }
        p.pendingProposals = nil
    }
}
```

**Note on follower committed entries**: Followers do not propose, so `pendingProposals` is always empty on followers. The FIFO dequeue only fires on the leader, which is correct.

**Note on no-op entries**: Leader election no-ops and conf changes are NOT proposed via `propose()`, so they should NOT dequeue from `pendingProposals`. The updated code skips them with `continue`.

### Test Plan

1. **Unit test: batched proposals** -- Send 5 proposals to the mailbox before any tick fires, verify all 5 callbacks receive results (not just the last one).
2. **Unit test: leader change drains proposals** -- Propose on a leader, then cause an election timeout; verify pending callbacks receive `ErrNotLeader`.
3. **Existing tests**: Run `TestPDRaftPeer_ProposeAndWait` and `TestPDRaftPeer_ProposalRejectedOnFollower` (`internal/pd/raft_peer_test.go:181,233`) to verify no regression.
4. **Integration test**: PD cluster e2e tests in `e2e_external/pd_cluster_integration_test.go`.

---

## Issue 2: PD C4 -- Snapshot Does Not Include storeLastHeartbeat

### Problem

The `PDSnapshot` struct (`internal/pd/snapshot.go:18-29`) captures Stores, Regions, Leaders, StoreStats, StoreStates, NextID, TSOState, GCSafePoint, and PendingMoves -- but **not** `storeLastHeartbeat`.

The `MetadataStore` struct (`internal/pd/server.go:925`) tracks `storeLastHeartbeat map[uint64]time.Time`, which is used by `IsStoreAlive()` (line 1013), `GetDeadStores()` (line 1076), and `checkStoreState()` to determine store liveness.

When a follower applies a snapshot:
1. It receives store state `StoreStateUp` for live stores (from `StoreStates`)
2. But `storeLastHeartbeat` remains an empty map (never populated from snapshot)
3. `checkStoreState()` / `IsStoreAlive()` checks `storeLastHeartbeat[storeID]` and finds no entry
4. The store appears as if it never sent a heartbeat, causing it to be classified as Down/Disconnected

After leadership transfer to the snapshot-restored follower, the new leader immediately considers all stores unhealthy, potentially triggering unnecessary replica repair scheduling.

### Current Code

```go
// internal/pd/snapshot.go:18-29
type PDSnapshot struct {
    Bootstrapped bool                        `json:"bootstrapped"`
    Stores       map[uint64]*metapb.Store    `json:"stores"`
    Regions      map[uint64]*metapb.Region   `json:"regions"`
    Leaders      map[uint64]*metapb.Peer     `json:"leaders"`
    StoreStats   map[uint64]*pdpb.StoreStats `json:"store_stats"`
    StoreStates  map[uint64]StoreState       `json:"store_states"`
    NextID       uint64                      `json:"next_id"`
    TSOState     TSOSnapshotState            `json:"tso_state"`
    GCSafePoint  uint64                      `json:"gc_safe_point"`
    PendingMoves map[uint64]*PendingMove     `json:"pending_moves"`
}
```

`GenerateSnapshot()` (line 33-91) copies all fields but omits `storeLastHeartbeat`.
`ApplySnapshot()` (line 95-151) restores all fields but never touches `storeLastHeartbeat`.

### Design

Add `StoreLastHeartbeat map[uint64]int64` to `PDSnapshot`. Use `int64` (Unix nanoseconds) for JSON serialization instead of `time.Time` (which serializes as a string and is fragile across timezones).

In `GenerateSnapshot()`, convert `time.Time` to `UnixNano()`.
In `ApplySnapshot()`, convert `int64` back to `time.Time` via `time.Unix(0, nanos)`.

### Implementation Steps

1. **Add field to `PDSnapshot`** (`snapshot.go:29`):

```go
type PDSnapshot struct {
    // ... existing fields ...
    PendingMoves       map[uint64]*PendingMove `json:"pending_moves"`
    StoreLastHeartbeat map[uint64]int64        `json:"store_last_heartbeat"`
}
```

2. **Update `GenerateSnapshot()`** (`snapshot.go:33-91`) -- add after the `storeStates` copy (line 58-60):

```go
// Inside s.meta.mu.RLock() block, after storeStates copy:
snap.StoreLastHeartbeat = make(map[uint64]int64, len(s.meta.storeLastHeartbeat))
for k, v := range s.meta.storeLastHeartbeat {
    snap.StoreLastHeartbeat[k] = v.UnixNano()
}
```

3. **Update `ApplySnapshot()`** (`snapshot.go:95-151`) -- add after `storeStates` restore (line 120-123):

```go
// After s.meta.storeStates restore:
s.meta.storeLastHeartbeat = make(map[uint64]time.Time, len(snap.StoreLastHeartbeat))
for k, v := range snap.StoreLastHeartbeat {
    s.meta.storeLastHeartbeat[k] = time.Unix(0, v)
}
```

4. **Add `"time"` import** to `snapshot.go` if not already present.

### Test Plan

1. **Unit test**: Create a PD server, register stores, send heartbeats, generate snapshot. Create a second PD server, apply the snapshot, and verify `IsStoreAlive()` returns `true` for all stores.
2. **Unit test**: Verify that after snapshot apply, `GetDeadStores()` returns empty (all stores are alive with recent heartbeats).
3. **Snapshot round-trip test**: Generate snapshot, marshal, unmarshal, apply, then re-generate. Compare `StoreLastHeartbeat` values match (within nanosecond precision).
4. **Backward compatibility**: Verify that applying a snapshot JSON without the `store_last_heartbeat` field does not crash (the field will be nil, and the code initializes from `len(nil)` which is 0).

---

## Issue 3: PD M1 -- TSO Logical Overflow May Produce Non-Monotonic Timestamps

### Problem

The `TSOAllocator.Allocate()` method (`internal/pd/server.go:1194-1215`) has a logical overflow bug:

```go
// internal/pd/server.go:1194-1215
func (t *TSOAllocator) Allocate(count int) (*pdpb.Timestamp, error) {
    t.mu.Lock()
    defer t.mu.Unlock()

    now := time.Now().UnixMilli()
    if now > t.physical {
        t.physical = now
        t.logical = 0
    }

    t.logical += int64(count)
    if t.logical >= (1 << 18) {
        // Overflow: advance physical.
        t.physical++
        t.logical = int64(count)
    }

    return &pdpb.Timestamp{
        Physical: t.physical,
        Logical:  int64(t.logical),
    }, nil
}
```

**Bug scenario**: Suppose `t.physical = 1000`, `t.logical = 262140` (just under 2^18 = 262144), and `count = 10`:

1. `now` is checked -- assume `now <= t.physical`, so no update.
2. `t.logical += 10` --> `t.logical = 262150` (exceeds 2^18).
3. Overflow branch: `t.physical++` --> `t.physical = 1001`.
4. `t.logical = int64(count)` --> `t.logical = 10`.
5. Returns `{Physical: 1001, Logical: 10}`.

Now consider the **previous** call returned `{Physical: 1001, Logical: 5}` (if physical had been advanced to 1001 by `now` in a prior call with logical = 5). The new timestamp `{1001, 10}` would be fine.

But the real problem is: if `t.physical++` sets `t.physical` to a value **in the future** relative to wall clock time, then in a subsequent call, `now < t.physical` so the physical won't advance, and we continue allocating with `logical = count`. This is correct for monotonicity but the physical value is now artificial.

The more serious bug: `t.physical++` advances by only 1 millisecond. If the current wall clock is already at `t.physical` or beyond, then `now > t.physical` in the next call will set `t.physical = now` and `t.logical = 0`, which produces `{now, count}`. If the previous call returned `{t.physical_old+1, count_old}` and `now == t.physical_old + 1`, then we get `{now, count}` which could be `{t.physical_old+1, count}`. If `count < count_old` from the overflow-triggered call, the logical part is smaller. But since physical is equal, the overall timestamp is `{same_physical, smaller_logical}` -- **non-monotonic**.

**Concrete example**:
- Call A: `physical=1000, logical=262140`. Allocate(10): overflow triggers, `physical=1001, logical=10`. Returns `{1001, 10}`.
- Call B immediately after: `now=1001` (wall clock caught up). `now > t.physical` is `1001 > 1001` = false. So `logical += 5` --> `logical=15`. Returns `{1001, 15}`. This is fine.

Actually the `now > t.physical` check (strict greater-than) means if `now == t.physical`, logical continues accumulating. The issue is when `t.physical++` advances to a value still less than `now`:

- `physical=999, logical=262140`. `now=1001`. `now > physical` --> `physical=1001, logical=0`.
- `logical += 10` = 10. No overflow. Returns `{1001, 10}`. Fine.

The actual non-monotonicity occurs in this edge case:
- `physical=1000, logical=262140`. `now=1000` (same ms). `now > physical` is false.
- `logical += 10 = 262150 >= 2^18`. Overflow: `physical=1001, logical=10`. Returns `{1001, 10}`.
- Next call: `now=1001`. `now > physical` is `1001 > 1001` = false. `logical += count`. Fine.
- Next call: `now=1002`. `now > physical` is `1002 > 1001` = true. `physical=1002, logical=0, logical += count`. Returns `{1002, count}`. This is > `{1001, 10}`. Fine.

The **actual** problem is when `now` is retrieved as a value **less** than the `physical+1` result, and a subsequent call resets `logical = 0`. But since `now > t.physical` uses strict greater-than, and `t.physical` was incremented, `now` would have to exceed `t.physical` (the incremented value) to reset -- which only happens when the real clock advances. At that point `physical` is higher, so monotonicity holds.

**Revised analysis**: The monotonicity is actually preserved in the simple case. The real bug is:

1. **`t.physical++` doesn't use `max(physical+1, now)`**: If wall clock is already ahead (e.g., `now = physical + 50`), incrementing physical by 1 creates a timestamp in the past relative to wall clock. Subsequent calls with `now > t.physical` will jump forward, wasting logical space.

2. **`t.logical = int64(count)` instead of `t.logical = 0` then `t.logical += count`**: This is actually fine (equivalent), but less clear.

3. **The `count` parameter can itself exceed 2^18**: If `count >= 2^18`, the overflow branch sets `t.logical = count`, which still exceeds 2^18. The function returns a logical value >= 2^18, which violates the TSO encoding format (TiKV uses 18 bits for logical).

### Design

Fix the overflow handling to:
1. Use `max(t.physical+1, now)` when advancing physical on overflow.
2. After advancing physical, reset logical to 0 and then add count.
3. Loop the overflow check in case `count` alone exceeds 2^18 (defensive; in practice count is small).

### Current Code

```go
// internal/pd/server.go:1194-1215
func (t *TSOAllocator) Allocate(count int) (*pdpb.Timestamp, error) {
    t.mu.Lock()
    defer t.mu.Unlock()

    now := time.Now().UnixMilli()
    if now > t.physical {
        t.physical = now
        t.logical = 0
    }

    t.logical += int64(count)
    if t.logical >= (1 << 18) {
        t.physical++
        t.logical = int64(count)
    }

    return &pdpb.Timestamp{
        Physical: t.physical,
        Logical:  int64(t.logical),
    }, nil
}
```

### Implementation Steps

1. **Update `Allocate()`** (`server.go:1194-1215`):

```go
func (t *TSOAllocator) Allocate(count int) (*pdpb.Timestamp, error) {
    if count <= 0 {
        return nil, fmt.Errorf("tso: count must be positive, got %d", count)
    }

    t.mu.Lock()
    defer t.mu.Unlock()

    now := time.Now().UnixMilli()
    if now > t.physical {
        t.physical = now
        t.logical = 0
    }

    t.logical += int64(count)

    // If logical overflows the 18-bit space, advance physical and reset.
    if t.logical >= (1 << 18) {
        // Advance physical to at least now+1 to maintain monotonicity
        // and avoid creating timestamps in the past.
        next := t.physical + 1
        now = time.Now().UnixMilli()
        if now >= next {
            next = now
        }
        t.physical = next
        t.logical = int64(count)
    }

    return &pdpb.Timestamp{
        Physical: t.physical,
        Logical:  t.logical,
    }, nil
}
```

2. **Add input validation** for `count` to prevent misuse (count > 2^18 would require multiple physical advances; reject or loop).

3. **Remove redundant cast**: Line 1213 has `Logical: int64(t.logical)` but `t.logical` is already `int64`. Remove the cast.

### Test Plan

1. **Unit test: normal allocation** -- Allocate several batches, verify each timestamp is strictly greater than the previous.
2. **Unit test: overflow** -- Set `logical` to `(1<<18) - 5`, allocate 10. Verify physical advances and logical is `10`. Verify the returned timestamp is greater than the pre-overflow timestamp.
3. **Unit test: large count** -- Allocate with `count = 1` after overflow to verify monotonicity.
4. **Unit test: count validation** -- Allocate with `count = 0` and `count = -1`, verify error returned.
5. **Stress test**: Run 1000 concurrent allocations in goroutines, collect all timestamps, sort, verify strict monotonicity (each timestamp > previous).
6. **Existing tests**: Run `internal/pd/server_test.go` tests to verify no regression.

---

## Files Modified

| File | Lines | Change |
|------|-------|--------|
| `internal/pd/raft_peer.go` | 98, 234-256, 268-278, 312-356 | Replace map-based proposal tracking with FIFO queue; drain on leader loss |
| `internal/pd/snapshot.go` | 18-29, 33-91, 95-151 | Add `StoreLastHeartbeat` field; serialize/deserialize in Generate/Apply |
| `internal/pd/server.go` | 1194-1215 | Fix TSO overflow: use `max(physical+1, now)`, add count validation |

## Risk Assessment

- **PD C1 (FIFO queue)**: Low risk. The FIFO approach is simpler than the map approach and relies on Raft's ordering guarantee. The key correctness property is that no-op and conf-change entries must NOT dequeue from the proposals slice, since they were not proposed via `propose()`.
- **PD C4 (snapshot heartbeat)**: Low risk. Additive change to snapshot format. Old snapshots without the field will deserialize with a nil map, and the code handles `len(nil)` gracefully.
- **PD M1 (TSO overflow)**: Medium risk. TSO is critical for transaction correctness. The fix must preserve strict monotonicity under all timing conditions. Thorough testing required.

---

## Addendum: Review Feedback Incorporated

**Review verdicts:**
- **PD C1 (FIFO Proposal Tracking): NEEDS REVISION** -- blocking issue addressed below.
- **PD C4 (Snapshot Heartbeat): PASS** -- no changes needed.
- **PD M1 (TSO Overflow): PASS** -- minor notes incorporated.

### Fix for PD C1: Route CmdCompactLog through `propose()` with nil callback -- BLOCKING

The FIFO tracking design has a critical flaw: `onRaftLogGCTick()` calls `rawNode.Propose()` directly (raft_peer.go:411), bypassing `propose()`. This means `CmdCompactLog` entries are committed without a corresponding entry in the `pendingProposals` queue. When `handleReady()` processes a committed `CmdCompactLog` entry, the code dequeues the next callback from the FIFO queue -- but that callback belongs to a *different* proposal. This causes callback mismatches: the wrong caller gets the wrong result, and a subsequent legitimate proposal's callback is silently lost.

`CmdCompactLog` entries are normal `EntryNormal` entries with valid data that successfully unmarshal as `PDCommand`, so they pass all the skip checks (non-empty data, not a conf change) and incorrectly trigger a dequeue.

**Chosen fix (Option 1 -- route through propose):**

Change `onRaftLogGCTick()` to send through the mailbox as a `PDProposal` with `Callback: nil`, instead of calling `rawNode.Propose()` directly:

```go
// In onRaftLogGCTick(), instead of:
//   p.rawNode.Propose(data)
// Use:
p.handleMessage(PDRaftMsg{
    Type: PDRaftMsgTypeProposal,
    Data: &PDProposal{
        Command:  compactLogCmd,
        Callback: nil,
    },
})
```

In `propose()`, always append to the slice even when callback is nil:

```go
func (p *PDRaftPeer) propose(proposal *PDProposal) {
    data, err := proposal.Command.Marshal()
    if err != nil {
        if proposal.Callback != nil {
            proposal.Callback(nil, fmt.Errorf("pd: marshal command: %w", err))
        }
        return
    }

    if err := p.rawNode.Propose(data); err != nil {
        if proposal.Callback != nil {
            proposal.Callback(nil, fmt.Errorf("pd: propose: %w", err))
        }
        return
    }

    // Always append to maintain FIFO alignment, even if callback is nil.
    // Nil callbacks are discarded during dequeue.
    p.pendingProposals = append(p.pendingProposals, proposal.Callback)
}
```

In `handleReady()`, the dequeue code handles nil callbacks naturally:

```go
// Dequeue the next pending callback (FIFO).
if len(p.pendingProposals) > 0 {
    cb := p.pendingProposals[0]
    p.pendingProposals = p.pendingProposals[1:]
    if cb != nil {
        cb(result, applyErr)
    }
}
```

This keeps the FIFO design intact: every entry proposed through `propose()` has a corresponding slot in the queue (nil or non-nil), and every committed entry that passes the skip checks dequeues exactly one slot.

**Alternative (Option 2)**: Use proposal IDs in serialized data instead of FIFO ordering. Embed a monotonic `proposalID` as a uint64 prefix before the `PDCommand` bytes. Only dequeue when the committed entry carries a non-zero proposal ID matching the head of the queue. This is more robust but requires a wire format change.

**Risk assessment correction**: The original doc says "Low risk." Given the `CmdCompactLog` interaction, this should be **Medium risk**. With the fix above applied, risk is reduced back to Low.

### Note on PD C4 (Snapshot Heartbeat)

Review passed. The snapshot is taken under `s.meta.mu.RLock()`, and `storeLastHeartbeat` is also protected by this same lock (written in `UpdateStoreStats` under `s.meta.mu.Lock()`), so the snapshotting is consistent.

### Note on PD M1 (TSO Overflow)

Review passed. Minor note: the fix re-reads `time.Now().UnixMilli()` inside the overflow branch. A code comment should explain why (to avoid creating timestamps behind the wall clock). The `count <= 0` validation is a good defensive addition. The coding agent should carry forward this rationale as an inline comment.
