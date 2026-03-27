# Design Review Notes

---

## Review of design_01_proposal_callback.md

**Verdict: NEEDS REVISION**

### Feasibility

The core idea (replacing index-based proposal tracking with a monotonic proposal ID embedded in entry data) is sound and aligns with how TiKV handles proposal correlation. The codebase analysis confirms all four problems described in the document are real:

- `pendingProposals` does use `lastIdx+1` (peer.go line 518-519), and batched proposals will indeed collide.
- Callback always passes `nil` success (peer.go line 522).
- `ProposeModifies` blocks until timeout on propose failure (coordinator.go lines 317-342).
- `applyFunc` receives all `rd.CommittedEntries` unfiltered (peer.go line 592).

### Concerns

**1. CompactLog entries will be corrupted by the entry filter (Section E) -- BLOCKING**

Section E proposes filtering entries for `applyFunc` with:
```go
if e.Type == raftpb.EntryNormal && !IsSplitAdmin(e.Data) && len(e.Data) > 8
```

This filter misses CompactLog entries. CompactLog entries are `EntryNormal`, start with tag `0x01` (not `0x02`, so `IsSplitAdmin` returns false), and are 17 bytes long (`len(e.Data) > 8` is true). They would pass the filter and be sent to `applyFunc`. The `applyEntriesForPeer` function (coordinator.go line 202) would then try to strip 8 bytes and unmarshal the remaining 9 bytes as `RaftCmdRequest`, producing garbage or a silent `continue`.

Currently, CompactLog entries reach `applyEntriesForPeer` but fail protobuf unmarshaling and are silently skipped. With the 8-byte stripping, the behavior is undefined.

**Fix:** Add an `IsCompactLog(data []byte) bool` helper (check `len(data) >= 1 && data[0] == 0x01`) or generalize to `IsAdminEntry(data []byte) bool` that checks for any admin tag byte (`0x01` or `0x02`). Update the filter:
```go
if e.Type == raftpb.EntryNormal && !IsSplitAdmin(e.Data) && !IsCompactLog(e.Data) && len(e.Data) > 8
```

Alternatively, since all non-`RaftCmdRequest` entries use a tag byte in `data[0]` and `RaftCmdRequest` starts with protobuf field 1 wire type 2 (`0x0A`), the filter could positively identify `RaftCmdRequest` entries. This is more future-proof if additional admin entry types are added.

**2. `propose()` is not the only Propose path -- document the asymmetry**

The `propose()` method (peer.go line 496) is only used for client `RaftCommand` proposals. Other entries are proposed directly via `p.rawNode.Propose()`:
- CompactLog at peer.go line 737
- ReadIndex no-op at peer.go line 839
- SplitAdmin via `ProposeSplit()` at split_admin.go line 168

These entries will NOT have the 8-byte proposal ID prefix. This is acceptable since they don't use callbacks, but the document should explicitly state this asymmetry and ensure all commit-time parsing guards against entries without the prefix. The `len(e.Data) < 8` check in Section D handles this correctly, but it should be more clearly documented as covering these non-prefixed entry types, not just "entries proposed by other leaders."

**3. `errorResponse` helper is referenced but never defined**

Sections B, C, and D call `errorResponse(err)` to build a `*raft_cmdpb.RaftCmdResponse` with an error header. Step 2 says to add it to `internal/raftstore/msg.go`, but the function body is never shown. The implementation needs to be specified since the `RaftCmdResponse` and its error header structure are defined by the kvproto package and have a specific format (`resp.Header.Error.Message`).

**4. `RaftCommand.Callback(nil)` nil-safety in `ProposeModifies`**

The success path calls `Callback(nil)` (a nil `*raft_cmdpb.RaftCmdResponse`). Section F's updated `ProposeModifies` callback checks `resp != nil && resp.Header != nil && resp.Header.Error != nil`. This is correct, but it should be explicitly called out as a design contract: success = `Callback(nil)`, failure = `Callback(errorResponse(err))`.

**5. Minor: `p.rawNode.Status().Term` performance in `propose()`**

`Status()` in etcd/raft v3.5 acquires a lock and copies the entire Raft status. Since `propose()` can be called frequently, consider caching the term from `handleReady()` where `rd.HardState.Term` is already available.

### Completeness

- All affected call sites for `pendingProposals` are covered.
- The `applyEntriesForPeer` update correctly strips the 8-byte prefix (modulo the CompactLog issue above).
- Step 5 (stale proposal cleanup) is well-designed with both leader-stepdown and periodic sweep.
- Test plan is thorough.

### Risk

**Data format change is not backward-compatible.** Existing Raft log entries do not have the 8-byte prefix. If a node restarts and replays old entries from the log, `applyEntriesForPeer` would strip 8 bytes from entries that don't have the prefix. Since the Raft log is ephemeral and entries are applied on commit (and the applied index is persisted), this only matters during a rolling upgrade. The design should document this as requiring a clean restart or add a guard that checks whether the entry has a proposal ID prefix before stripping.

---

## Review of design_02_transport_stream.md

**Verdict: NEEDS REVISION**

### Feasibility

The three problems are accurately identified and confirmed by code inspection:

- `Send()` creates a new gRPC stream per message (transport.go lines 78-102) -- confirmed.
- `newConnPool(addr, 1)` ignores `cfg.PoolSize` (transport.go line 249) -- confirmed. Also confirmed that `NewRaftClient` does not store `cfg.PoolSize` in the struct (lines 69-74), so `c.poolSize` does not exist.
- Loopback routing uses the source `regionID` (coordinator.go line 775) -- confirmed.

### Concerns

**1. `poolSize` field missing from `RaftClient` struct definition in Section A -- BLOCKING**

The design says to add `poolSize int` to `RaftClient` and references `c.poolSize` in Section B and Implementation Step 3. However, the struct definition shown in Section A only adds `streams` and `streamBuf`, omitting `poolSize`. The `NewRaftClient` code block in Section B does include `poolSize: cfg.PoolSize`, but the struct definition is inconsistent.

The current `RaftClient` struct (transport.go lines 21-27) has no `poolSize` field. The current `NewRaftClient` (lines 69-74) validates `cfg.PoolSize` but never stores it. Both the struct definition and the constructor must be updated.

**Fix:** Add `poolSize int` to the `RaftClient` struct definition in Section A alongside `streams` and `streamBuf`.

**2. Stream `Close()` ordering may panic or leak**

The `raftStream.Close()` method sets `closed = true`, then calls `close(rs.sendCh)`, then `rs.cancel()`. If `sendLoop` is blocked on `rs.stream.Send(msg)` (a blocking gRPC call), `close(rs.sendCh)` does not unblock it. The `rs.cancel()` cancels the context, which should cause `stream.Send()` to return with an error, but the goroutine will then try to range over a closed channel. Additionally, if `sendLoop` is between receiving from `sendCh` and calling `stream.Send`, and `Close()` runs concurrently, the `close(sendCh)` is safe because `sendLoop` ranges over the channel.

However, there is still a potential issue: if `Send()` is called concurrently with `Close()`, the `closed.Load()` check in `Send()` might pass, then `Close()` runs and closes `sendCh`, then `Send()` tries to write to a closed channel, causing a panic.

**Fix:** Use a select-based send in `raftStream.Send()` instead of direct channel write, or add a mutex around the close-and-send path. The simplest fix:
```go
func (rs *raftStream) Send(msg *raft_serverpb.RaftMessage) error {
    if rs.closed.Load() {
        return fmt.Errorf("stream closed")
    }
    defer func() {
        if r := recover(); r != nil {
            // Channel was closed between the check and the send.
        }
    }()
    select {
    case rs.sendCh <- msg:
        return nil
    default:
        return fmt.Errorf("stream send buffer full for store %d", rs.storeID)
    }
}
```
Or better, never close `sendCh` and use context cancellation to stop the `sendLoop`:
```go
func (rs *raftStream) sendLoop() {
    for {
        select {
        case msg := <-rs.sendCh:
            if err := rs.stream.Send(msg); err != nil {
                rs.closed.Store(true)
                return
            }
        case <-rs.ctx.Done():
            return
        }
    }
}
func (rs *raftStream) Close() {
    rs.closed.Store(true)
    rs.cancel()
}
```

**3. Stream reconnection drops buffered messages**

When `getOrCreateStream` detects a closed stream and creates a new one, the old `sendCh` may still have buffered messages that are lost. The design should explicitly document that messages in the old buffer are dropped. This is acceptable since Raft is idempotent and retries, but it should be noted.

**4. Peer-to-region mapping maintenance is incomplete**

The design lists three maintenance points for `RegisterPeer`/`UnregisterPeer`:
- `BootstrapRegion()` -- register initial peers
- Split handler -- register new peers
- `RemovePeer()` -- unregister removed peers

Missing maintenance points:
- **`CreatePeer()` (coordinator.go line ~488)** -- when a new peer is created via `maybeCreatePeerForMessage`, the mapping must be updated.
- **`DestroyPeer()` (coordinator.go)** -- when a peer is destroyed, unregister its mapping.
- **ConfChange application** -- when peers are added/removed via Raft config changes, the mapping must be updated.

**5. `RaftClient.Close()` must also close streams**

Implementation Step 5 mentions this, but the code for the updated `Close()` is not provided. The current `Close()` (transport.go lines 210-218) only closes connection pools. The implementation should show:
```go
func (c *RaftClient) Close() {
    c.mu.Lock()
    defer c.mu.Unlock()
    for _, rs := range c.streams {
        rs.Close()
    }
    c.streams = make(map[uint64]*raftStream)
    for _, pool := range c.connections {
        pool.close()
    }
    c.connections = make(map[uint64]*connPool)
}
```

**6. Connection pool round-robin vs. long-lived streams interaction (minor)**

The design adds both connection pool round-robin and long-lived streams. A stream is bound to one connection. If the pool has multiple connections, the stream only uses one. The round-robin in the pool is useful for `BatchSend` and `SendSnapshot`, but the single stream per store makes the pool less relevant for normal `Send`. This isn't a bug, but the interaction should be documented.

### Completeness

- The test plan covers stream reuse, reconnection, buffer overflow, pool round-robin, and loopback routing.
- The fallback behavior for loopback routing (when `FindRegionByPeerID` fails) is correct.

### Risk

- **Low backward compatibility risk** for stream reuse (pure client-side optimization).
- **Medium risk for loopback routing change** -- if the peer-to-region mapping is not kept perfectly in sync, messages could be misrouted. The fallback to source `regionID` mitigates this for pre-split scenarios.

---

## Review of design_03_latch_blocking.md

**Verdict: NEEDS REVISION**

### Feasibility

All three problems are confirmed by code inspection:

- 21 spin-wait call sites in storage.go (confirmed by grep).
- `wakeCh` is allocated in `New()` (latch.go line 43) but never read from or written to anywhere -- safe to remove.
- `GCWorker.applyModifies` writes directly via `WriteBatch` without latches (gc.go lines 284-302).
- GC keeps Delete markers unconditionally in `gcStateRemoveIdempotent` (gc.go lines 100-104).

### Concerns

**1. `AcquireBlocking` has a race between `Acquire()` failure and channel creation -- BLOCKING**

The current design creates the wake channel AFTER `Acquire()` fails:
```go
if l.Acquire(lock, commandID) {
    l.waiters.Delete(commandID)
    return nil
}
ch := make(chan struct{}, 1)
if actual, loaded := l.waiters.LoadOrStore(commandID, ch); loaded {
    ch = actual.(chan struct{})
}
select { case <-ch: ... }
```

There is a window between `Acquire()` returning false (which adds the command to the slot's `waitQueue`) and the channel being stored in `waiters`. During this window, `Release()` on another goroutine could:
1. Release the slot.
2. Find the command in the `wakeUp` list.
3. Look for it in `waiters` -- find nothing (channel not stored yet).
4. The wake-up signal is lost.

The command then blocks forever (or until context timeout).

**Fix:** Create and store the channel BEFORE calling `Acquire()`:
```go
func (l *Latches) AcquireBlocking(ctx context.Context, lock *Lock, commandID uint64) error {
    ch := make(chan struct{}, 1)
    l.waiters.Store(commandID, ch)

    for {
        if l.Acquire(lock, commandID) {
            l.waiters.Delete(commandID)
            return nil
        }
        select {
        case <-ch:
            continue
        case <-ctx.Done():
            l.Release(lock, commandID)
            l.waiters.Delete(commandID)
            return ctx.Err()
        }
    }
}
```

This is safe because:
- If `Acquire()` succeeds, the channel is cleaned up and never used.
- If `Acquire()` fails, the channel is already in place for `Release()` to find.
- The channel buffer size of 1 ensures a signal is not lost even if it arrives before the `select`.

**2. Stale waitQueue entries after timeout (minor)**

When `AcquireBlocking` times out, `Release()` only removes owned slots (where `slot.owner == commandID`). It does NOT remove the command from the `waitQueue` of the slot where acquisition failed. This means:
- The timed-out command ID stays in the `waitQueue`.
- When that slot is later released, the timed-out command is dequeued and signaled.
- Since `waiters.Delete(commandID)` already ran, `Release()` finds no channel, and the signal is dropped.
- But the next real waiter is delayed by one dequeue cycle.

This is a minor inefficiency, not a correctness bug. Under high timeout rates it could delay legitimate waiters. Documenting this trade-off is sufficient for now.

**3. GCWorker `nextCmdID` conflicts with Storage `nextCmdID` -- BLOCKING**

The design adds `nextCmdID uint64` (atomic counter) to `GCWorker` for latch command IDs. The `Storage` struct also has `nextCmdID` (storage.go line 26), starting from 1. If both counters produce the same command ID, the latch system treats them as the same command, causing incorrect "already owned" results in `Acquire()`.

**Fix options:**
- **(a) Share a single counter.** Pass an `*atomic.Uint64` to both `Storage` and `GCWorker`. Cleanest but requires refactoring `Storage.allocCmdID()` to use atomic operations instead of `sync.Mutex`.
- **(b) Partition the ID space.** Start the GCWorker counter at `1 << 32` (4 billion+) to avoid overlap with Storage's counter for practical purposes.
- **(c) Use odd/even IDs.** Storage uses even IDs, GCWorker uses odd. Requires changing `allocCmdID`.

Option (a) is recommended for correctness. Option (b) is simpler but technically allows overlap after billions of operations.

**4. `NewGCWorker` call sites -- private `latches` field access**

The design says "The `Storage` struct already holds `*latch.Latches`, so pass `s.latches`." However, `latches` is a private field (lowercase) on `Storage`. Either:
- Add a `Latches() *latch.Latches` accessor method to `Storage`, or
- Pass the latches at a higher level where both are constructed.

In `internal/server/server.go:76`, the GCWorker is created as:
```go
gcWorker := gc.NewGCWorker(storage.Engine(), gc.DefaultGCConfig())
```
To pass latches, this would become:
```go
gcWorker := gc.NewGCWorker(storage.Engine(), storage.Latches(), gc.DefaultGCConfig())
```
This requires adding `func (s *Storage) Latches() *latch.Latches { return s.latches }`.

There is also a test call site at `e2e/gc_worker_test.go:35` that would need updating. Consider making the `latches` parameter optional (accept `nil` and create a local instance for testing).

**5. 21 call sites -- error handling guidance needed**

Each spin-wait site needs conversion. The "appropriate error" return differs per method:
- Methods returning `error` can return `fmt.Errorf("latch acquire: %w", err)`
- Methods returning `(*T, error)` can return `nil, fmt.Errorf(...)`
- Methods returning `(*T, *LatchGuard, error)` can return `nil, nil, fmt.Errorf(...)`

Since the initial conversion uses `context.Background()` (no timeout), the error path is currently unreachable, making this mostly a mechanical change. However, the document should note this and recommend a follow-up to add proper context propagation for future cancellation support.

**6. Delete marker GC peek-ahead uses same reader/snapshot (no issue)**

The `reader.SeekWrite(key, commitTS-1)` call in Section C uses the same snapshot-backed reader. This is correct (consistent view). Performance impact of extra `SeekWrite` calls is negligible since Delete markers are typically rare.

### Completeness

- All 21 spin-wait sites are identified with correct line numbers.
- GC latching is well-designed with per-batch latch acquisition.
- Delete marker GC covers both the simple case (orphaned Delete, no older versions) and the same-pass optimization (remove Delete after cleaning all older versions below it).
- Test plan is comprehensive.

### Risk

- **Low risk for latch blocking** -- the change is a pure improvement. Blocking vs. spinning achieves the same serialization with better CPU efficiency.
- **Medium risk for GC latching** -- introduces a dependency between GCWorker and the latch subsystem. If the latch instance is not the SAME instance used by Storage, mutual exclusion is broken. This must be enforced by construction (shared pointer).
- **Low risk for Delete marker GC** -- only removes data below safe point with no references. The two-pass safety net (remove Delete marker on next GC pass if not caught in same pass) is a good fallback.

---

## Review of design_04_pd_proposal_tracking.md

### Issue 1: PD C1 -- FIFO Proposal Tracking

**Verdict: NEEDS REVISION**

The bug analysis is correct: `LastIndex()` returns the persisted index, not the unstable index, so batched proposals compute the same `expectedIdx` and overwrite each other's callbacks. However, the proposed FIFO solution has a critical flaw.

**Critical concern: `onRaftLogGCTick()` calls `rawNode.Propose()` directly (raft_peer.go:411), bypassing `propose()`.** This means `CmdCompactLog` entries are committed without a corresponding entry in the `pendingProposals` queue. When `handleReady()` processes a committed `CmdCompactLog` entry, the proposed code would dequeue the next callback from the FIFO queue -- but that callback belongs to a *different* proposal. This causes callback mismatches: the wrong caller gets the wrong result, and a subsequent legitimate proposal's callback is silently lost.

The design document's assertion that "no-op entries and conf-change entries must NOT dequeue" is correctly handled with `continue` statements, but it overlooks `CmdCompactLog` entries that arrive through the `rawNode.Propose()` backdoor. These are normal `EntryNormal` entries with valid data that successfully unmarshal as `PDCommand`, so they would pass all the skip checks and incorrectly trigger a dequeue.

**Concrete fix**: Either:
1. Route `CmdCompactLog` through `propose()` with a `nil` callback (so a nil entry is enqueued in the FIFO, and the dequeue simply discards it), OR
2. Use the proposal-ID-in-header approach originally discussed but dismissed. Embed a monotonic `proposalID` in the serialized data (e.g., a uint64 prefix before the `PDCommand` bytes). Only dequeue when the committed entry carries a non-zero proposal ID matching the head of the queue.

Option 1 is simpler and keeps the FIFO design intact. Change `onRaftLogGCTick()` to send through the mailbox as a `PDProposal` with `Callback: nil`, and update `propose()` to always append to the slice (appending nil is fine; the dequeue code already handles `len(pendingProposals) > 0` checks).

**Additional concern**: The current `handleReady()` code (lines 317-319) dequeues for empty (no-op) entries: `if cb, ok := p.pendingProposals[e.Index]; ok { cb(nil, nil) }`. The design says "skip, do NOT dequeue" for no-ops. This is correct for the FIFO approach since no-ops are not proposed via `propose()`. But the *existing* map-based code does try to look them up. Confirm this discrepancy is intentional.

**Risk assessment correction**: The doc says "Low risk." It should be "Medium risk" given the `CmdCompactLog` interaction. With the fix above, risk is reduced back to Low.

### Issue 2: PD C4 -- Snapshot Missing storeLastHeartbeat

**Verdict: PASS**

The analysis is accurate. Verified against actual code:
- `PDSnapshot` (snapshot.go:18-29) indeed omits `storeLastHeartbeat`.
- `GenerateSnapshot()` (snapshot.go:33-91) copies `storeStates` but not `storeLastHeartbeat`.
- `ApplySnapshot()` (snapshot.go:95-151) never touches `storeLastHeartbeat`.
- `updateStoreStates()` (server.go:1064-1093) checks `storeLastHeartbeat` and sets stores to `StoreStateDown` when not found.

The implementation steps are clear and correct. Using `int64` (UnixNano) for JSON serialization instead of `time.Time` is the right call. Backward compatibility with old snapshots (nil map, `len(nil) == 0`) is correctly handled.

One minor note: the snapshot is taken under `s.meta.mu.RLock()`, but `storeLastHeartbeat` is also protected by this same lock (written in `UpdateStoreStats` under `s.meta.mu.Lock()`), so the snapshotting is consistent. Good.

### Issue 3: PD M1 -- TSO Logical Overflow

**Verdict: PASS (with minor notes)**

The revised analysis in the document is thorough and eventually concludes that strict monotonicity is preserved in the common case. The real issues identified are:
1. `physical++` not using `max(physical+1, now)` -- wastes logical space.
2. `count >= 2^18` not handled.
3. Redundant cast on line 1213.

The proposed fix is correct and clean. Verified against actual code at `server.go:1194-1215` -- the code matches the document's listing exactly.

Minor note: The fix re-reads `time.Now().UnixMilli()` inside the overflow branch. This is fine but slightly unusual; a comment explaining why (to avoid creating timestamps behind wall clock) would help future readers. The doc does explain this reasoning, so the coding agent should carry the comment forward.

The `count <= 0` validation is a good defensive addition.

---

## Review of design_05_misc_fixes.md

### Issue 1: Client M4 -- LockResolver Map Memory Leak

**Verdict: PASS**

Verified against actual code (`lock_resolver.go:16-77`). The analysis is correct: Go maps never shrink their backing array. The proposed fix (counter + recreate when empty after threshold) is simple and safe. The threshold of 1000 is reasonable.

One minor observation: the design doc says "No change to `NewLockResolver()`" because `resolveCount` zero-value is correct. This is accurate -- `NewLockResolver()` returns `&LockResolver{...resolving: make(map[lockKey]chan struct{})}` and the zero-value `resolveCount = 0` works.

### Issue 2: Raft M4 -- scanRegionSize Split Key from First CF

**Verdict: NEEDS REVISION**

The analysis of the problem is correct: the current code picks the split key from whichever CF's cumulative contribution first crosses `halfSize`, which is influenced by the scan order rather than data distribution.

**Concerns with the proposed implementation:**

1. **The per-CF threshold `SplitSize/2` is wrong for non-dominant CFs.** The per-CF candidate is recorded when `info.size >= w.cfg.SplitSize/2`. For a CF that has only 10MB of data (well below `SplitSize/2` which might be ~48MB), no candidate is ever recorded. This is acknowledged in the doc ("only the dominant CF's candidate will be non-nil"). But if *no* CF individually exceeds `SplitSize/2`, no candidates are recorded at all, and the code falls through to the expensive two-pass fallback. This can happen when data is evenly spread across CFs (e.g., 30MB default + 30MB write + 30MB lock = 90MB total, but no single CF exceeds 48MB).

2. **The fallback performs a second full scan** of all CFs, doubling I/O. The document itself notes this concern but proceeds anyway. For a region near `MaxSize`, this is expensive.

3. **The `goto selectKey` on early exit (totalSize >= MaxSize) may leave iterators from previous CFs unclosed.** Actually, looking at the code, each CF's iterator is opened, iterated, and closed within the loop body, so only the current CF's iterator is open at the `goto`. But the `goto` jumps past the `iter.Close()` call for the current CF -- the iterator is closed inline before the goto (`iter.Close()` at the line before `goto selectKey`). This is actually correct in the proposed code.

4. **Simpler alternative not explored**: Instead of per-CF tracking, just use the existing single-pass approach but pick the split key from the *last* (most recent) CF's data near the global midpoint. Or better: track the CF identity of the current scan and record which CF the split key came from. After all scans, if the split key came from a minor CF, re-scan only the dominant CF. This avoids the always-two-pass fallback.

**Concrete fix**: Use a simpler heuristic: during the single-pass scan, record split key candidates at per-CF local midpoints (`cfSize/2` for each CF, computed on the fly as each CF's iteration progresses). After scanning all CFs, select the candidate from the CF with the largest `cfSize`. If no candidate exists for the dominant CF (because it has fewer entries than expected), fall back to the global midpoint candidate (the current behavior). This avoids the second scan entirely.

Specifically, change the per-CF threshold from `w.cfg.SplitSize/2` to `info.size >= info.size` -- i.e., record the candidate when `info.size` first exceeds half of the CF's *running* total. Since we cannot know the final total during iteration, use a different approach: always update `info.splitKey` to the key at position `cfSize/2`. This can be done by recording the midpoint candidate whenever `info.size` crosses `info.size/2` (which requires re-checking after each key). A simpler approach: use a reservoir sampling technique or just record the key at the first key that pushes `info.size` past the running `info.size / 2` checkpoint.

Actually the simplest correct approach: track per-CF size and pick the last key seen when `info.size` is approximately `info.size/2`. Since we cannot predict the final size, record the key every time `info.size` doubles (at 1x, 2x, 4x, etc.) and pick the one closest to half. This is over-engineered.

**Recommended simpler approach**: Keep the single-pass, but instead of recording the split key when `totalSize >= halfSize` (global), record per-CF: the key when `cfRunningSize >= cfRunningSize/2`. Since we don't know the final cf size, just record the split key at `cfSize >= totalRunningSize / 2` (i.e., when this CF's contribution pushes total past midpoint, exactly as current code does, but tagged with the CF). After scanning, if the tagged CF is not the dominant one, pick a key from the dominant CF's midpoint. If no key was recorded for the dominant CF, fall back to the globally selected key. This avoids the two-pass fallback for the common case.

### Issue 3: Raft M5 -- ExecCommitMerge Dead Code and Fallback

**Verdict: PASS**

The analysis is accurate. Verified against actual code (`merge.go:102-155`):
- Lines 118-121 are indeed a dead code block (empty if-body).
- The fallback branch (lines 130-140) silently extends the merged region to cover non-adjacent ranges, which would corrupt the region map.
- The proposed fix correctly replaces the fallback with an error return.

One edge case to note: the existing code at line 123 checks `len(source.GetStartKey()) > 0` before the right-merge comparison. This means if `source.GetStartKey()` is empty (source is the first region, starting at ""), the right-merge branch is skipped even if `target.GetEndKey()` is also empty (both are at the beginning of keyspace). The proposed code preserves this check, which is correct -- two regions both starting at "" cannot be right-adjacent. However, the proposed code should also handle the edge case where `target.GetEndKey()` is empty (target covers to end of keyspace) and `source.GetStartKey()` is empty (source covers from start of keyspace). In this case neither branch matches, and the proposed code returns an error. This is correct behavior since such regions are not adjacent (they overlap or are the same).

### Issue 4: Server P1 -- ReadPool.Stop() Drain

**Verdict: PASS**

Verified against actual code (`flow.go:37-125`). The analysis is correct. The proposed drain-on-stop approach is sound.

One minor observation: the proposed `Stop()` changes the method signature from returning nothing to blocking until drain completes (via `rp.wg.Wait()`). Callers of `Stop()` should be checked for compatibility. Grepping shows `Stop()` is only called in `rp.stopCh` close context, so blocking is fine.

The `WaitGroup` approach correctly ensures all workers have finished (including draining) before `Stop()` returns. The drain loop (`for { select { case task := <-rp.taskCh: ... default: return } }`) is correct -- it processes remaining tasks non-blockingly.

### Issue 5: m5 -- Replace grpc.Dial with grpc.NewClient

**Verdict: NEEDS REVISION**

The design doc lists 6 call sites for `grpc.Dial` replacement, but the actual codebase has significantly more `grpc.Dial`/`grpc.DialContext` usages in gookv source (excluding `tikv/`):

**Missing from the design doc** (non-tikv gookv source):
- `internal/pd/transport.go:121` -- uses `grpc.DialContext` (not `grpc.Dial`)
- `internal/server/transport/transport.go:284` -- uses `grpc.DialContext`
- `pkg/client/request_sender.go:153` -- uses `grpc.DialContext`
- `pkg/pdclient/client.go:157,211,240` -- uses `grpc.DialContext` with `grpc.WithBlock()`
- `scripts/txn-demo-verify/main.go:523` -- uses `grpc.DialContext` with `grpc.WithBlock()`
- `scripts/pd-failover-demo-verify/main.go:488` -- uses `grpc.DialContext` with `grpc.WithBlock()`
- `scripts/scale-demo-verify/main.go:391` -- uses `grpc.DialContext` with `grpc.WithBlock()`
- `scripts/txn-integrity-demo-verify/main.go:765,869,932` -- uses `grpc.DialContext` with `grpc.WithBlock()`
- `scripts/pd-cluster-verify/main.go:28,93,169` -- uses `grpc.DialContext` with `grpc.WithBlock()`

**Critical issue with `grpc.WithBlock()` callers**: The design doc states "all current usages ... don't use `grpc.WithBlock()`." This is incorrect. Multiple call sites use `grpc.WithBlock()`, notably `pkg/pdclient/client.go` (lines 159, 213, 242) and all verification scripts. `grpc.NewClient` does NOT support `grpc.WithBlock()` -- it is always non-blocking. Migrating these sites requires removing `grpc.WithBlock()` and changing the connection establishment pattern (e.g., using a health check or explicit `conn.WaitForReady()` at RPC time).

For the `pdclient` code specifically, the `grpc.WithBlock()` is used intentionally to ensure the connection is established before proceeding (e.g., to discover the cluster ID immediately after connecting). Simply removing `grpc.WithBlock()` would change semantics: `GetMembers()` at line 181 would fail if the connection hasn't been established yet (though gRPC RPCs do wait for connectivity by default with `WaitForReady`).

**Concrete fix**:
1. For the 6 sites listed in the doc (which use `grpc.Dial` without `grpc.WithBlock()`), the migration is indeed straightforward -- proceed as designed.
2. For `grpc.DialContext` sites that do NOT use `grpc.WithBlock()` (transport.go, request_sender.go), also migrate. Note that `grpc.DialContext` with a context timeout acts differently from `grpc.NewClient` -- the context timeout constrains connection establishment in `DialContext` but has no effect on `NewClient`. These sites should be migrated carefully.
3. For sites using `grpc.WithBlock()`, either defer migration or implement an alternative blocking mechanism (e.g., `conn.Connect()` followed by `conn.WaitForStateChange()`).
4. Scripts in `scripts/` should also be listed if the doc intends comprehensive coverage.

---

## Review of design_06_test_improvements.md

### Issue 1: Test 1.1 -- GC Test Missing Old Version Check

**Verdict: PASS**

Verified against actual code (`gc_worker_test.go:42-171`). The analysis is accurate:
- `TestGCWorkerCleansOldVersions` only checks that the new version at `Version: 50` returns `"new-val"` (line 96-99). It never checks that the old version at `Version: 25` is gone.
- `TestGCWorkerMultipleKeys` has the same gap (lines 161-168).

The proposed fix is correct. Reading at `Version: 25` (after commitTS 20 but before commitTS 40) should return `NotFound` after GC with safe point 35 has cleaned up the old version.

The negative validation suggestion (disabling GC to verify assertions fail) is good practice.

### Issue 2: Test 5.1 -- Duplicate Cluster Helpers

**Verdict: PASS (with minor note)**

Verified against actual code:
- `cluster_raw_kv_test.go:16-33` defines `newClusterWithLeader()`.
- `client_lib_test.go:17-36` defines `newClientCluster()`.
- Both are in package `e2e_external_test`.
- Both are functionally identical except `newClientCluster` also returns `cluster.RawKV()`.

The consolidation plan is clean: move both to `helpers_test.go`, have `newClientCluster` call `newClusterWithLeader`.

**Minor note**: The proposed `newClientCluster` calls `cluster.RawKV()` after `newClusterWithLeader(t)`. But `newClusterWithLeader` already calls `cluster.RawKV()` internally for the health check. If `cluster.RawKV()` creates a new client each time (rather than returning a cached one), the returned `RawKVClient` in `newClientCluster` would be a different instance from the one used for the health check. This is likely fine (both connect to the same cluster), but worth verifying that `cluster.RawKV()` returns the same instance. If it creates a new client each time, there could be two clients open, which is benign but slightly wasteful.

The import cleanup guidance (step 4, 5) is reasonable but underspecified. The coding agent should run `goimports` or `go build` to verify imports after the refactoring.

---

## Summary

| Document | Issue | Verdict |
|----------|-------|---------|
| design_01 | Proposal Callback Reliability | **NEEDS REVISION** -- CompactLog entries corrupted by entry filter; `errorResponse` undefined; backward compat risk |
| design_02 | Transport Stream Reuse | **NEEDS REVISION** -- `poolSize` field missing from struct; stream Close() race; incomplete peer mapping maintenance |
| design_03 | Latch Blocking and GC Safety | **NEEDS REVISION** -- Race in `AcquireBlocking` channel setup; command ID space collision between Storage and GCWorker |
| design_04 | PD C1 -- FIFO Proposal Tracking | **NEEDS REVISION** -- `onRaftLogGCTick` bypasses `propose()`, causing FIFO callback mismatch |
| design_04 | PD C4 -- Snapshot Heartbeat | PASS |
| design_04 | PD M1 -- TSO Overflow | PASS |
| design_05 | Client M4 -- LockResolver Map GC | PASS |
| design_05 | Raft M4 -- Split Key from Dominant CF | **NEEDS REVISION** -- threshold `SplitSize/2` fails when no single CF is dominant; fallback doubles I/O |
| design_05 | Raft M5 -- Merge Dead Code | PASS |
| design_05 | Server P1 -- ReadPool Drain | PASS |
| design_05 | m5 -- grpc.Dial Migration | **NEEDS REVISION** -- misses 12+ `grpc.DialContext` sites; incorrectly claims no `grpc.WithBlock()` usage |
| design_06 | Test 1.1 -- GC Old Version Check | PASS |
| design_06 | Test 5.1 -- Duplicate Helpers | PASS |
