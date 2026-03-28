# Review Notes: Performance Optimization Phase 1

---

## Review 2026-03-28 (v2) -- Source-verified design review

Reviewer examined all 4 design documents (`01_overview.md`, `02_propose_apply_pipeline.md`,
`03_raft_log_batch_write.md`, `04_migration_plan.md`) against the current gookv source at
commit `31b84b295`. All source line references verified by reading the actual files.

**Methodology:** Each design claim was cross-referenced against source code at:
- `internal/raftstore/peer.go` (handleReady at 542, onApplyResult at 710, Run at 312)
- `internal/raftstore/storage.go` (SaveReady at 215, SetAppliedIndex at 399, PersistApplyState at 429, appendToCache at 478)
- `internal/raftstore/msg.go` (ApplyResult at 74, proposalEntry at 192)
- `internal/server/coordinator.go` (BootstrapRegion at 114, CreatePeer at 497, Stop at 420)
- `internal/server/storage.go` (ApplyModifies at 75)
- `internal/engine/traits/traits.go` (WriteBatch interface at 72, Clear at 92, Commit at 104)
- `internal/server/flow/flow.go` (ReadPool at 38)

---

## Document Verdicts

| Document | Verdict |
|----------|---------|
| 01_overview.md | **PASS** |
| 02_propose_apply_pipeline.md | **NEEDS REVISION** |
| 03_raft_log_batch_write.md | **NEEDS REVISION (minor)** |
| 04_migration_plan.md | **NEEDS REVISION** |

---

## 01_overview.md -- PASS

No issues found. All line references are accurate. Architecture diagrams correctly
depict the current and target states.

- `storage.go:215-258` (SaveReady): Verified. Function at line 215, `wb.Commit()` at
  line 244, in-memory update at 248-255.
- `peer.go:598-653` (inline apply): Verified. CommittedEntries block at 598, applyFunc
  call at 621, SetAppliedIndex at 647, PersistApplyState at 650.
- `coordinator.go:32` (engine field): Verified.
- Dependency graph and implementation ordering are sound.

---

## 02_propose_apply_pipeline.md -- NEEDS REVISION

### Finding 1 (HIGH): Applied index stalls if last committed entry is admin

At peer.go:646-647, the current code sets the applied index to the last committed
entry's index regardless of type:
```go
lastEntry := rd.CommittedEntries[len(rd.CommittedEntries)-1]
p.storage.SetAppliedIndex(lastEntry.Index)
```

The design's `processTask` (section 5.3, line 339) computes `appliedIndex` from
`task.Entries[len(task.Entries)-1].Index`. But `task.Entries` contains only **data
entries** (admin entries are filtered out). If the last committed entry is a CompactLog
or SplitAdmin, the `ApplyResult.AppliedIndex` will be the last data entry's index, not
the last committed entry's index. This causes the applied index to stall, which in turn
breaks ReadIndex (reads wait forever for an applied index that never arrives).

**Fix:** Add a `LastCommittedIndex uint64` field to `ApplyTask`. Set it to
`rd.CommittedEntries[len(rd.CommittedEntries)-1].Index` in `handleReady()`. The apply
worker returns this value in `ApplyResult.AppliedIndex`. When the batch contains only
admin entries (no data entries to apply), the peer must update `appliedIndex` inline
rather than submitting an empty apply task.

### Finding 2 (HIGH): Per-region ordering gap -- buffering strategy unspecified

Section 6.3 states only one `ApplyTask` is outstanding per region, gated by
`applyInFlight bool`. However, the design does not specify what happens when
`handleReady()` is called again (peer.go:387 -- called on every event loop iteration)
while `applyInFlight` is true and new committed entries arrive.

The `Peer.Run()` event loop (peer.go:340-388) calls `handleReady()` after every message
batch. `rawNode.HasReady()` at line 543 will return true if there are new committed
entries. The peer calls `rawNode.Ready()` which consumes the ready from the RawNode.
Once consumed, the entries must be processed -- they cannot be "put back."

If `applyInFlight` is true, the peer has three options:
1. Skip the committed entries portion (dangerous -- entries are consumed and lost)
2. Buffer the entire Ready (complex -- must buffer entries, hard state, messages, etc.)
3. Block until the in-flight apply completes (defeats async purpose)

None of these are specified in the design. TiKV handles this by allowing multiple
apply tasks per region in a FIFO queue, with the apply FSM processing them sequentially.

**Fix:** Specify one of:
- (a) Per-region FIFO in the worker pool: allow multiple ApplyTasks for the same region,
  but ensure the worker processes them in submission order. Requires per-region queueing.
- (b) Accumulation in the peer: add `bufferedCommittedEntries []raftpb.Entry` to Peer.
  When `applyInFlight` is true, append new committed entries to the buffer. When
  `onApplyResult` fires, submit the buffer as the next ApplyTask. The `appliedIndex` in
  the ApplyResult must still reflect the full range including admin entries.

### Finding 3 (HIGH): Snapshot during in-flight apply causes data corruption

If the apply pool has an in-flight task when a snapshot Ready arrives, the snapshot
replaces all data in the region's key range via `ApplySnapshotData` (snapshot.go:263),
which uses `DeleteRange` + `Put`. If an apply worker is simultaneously writing to the
same key range, the results are undefined.

In `handleReady()`, snapshot application (line 586-590) occurs AFTER `SaveReady` but
BEFORE committed entries (line 598). However, the in-flight apply from the *previous*
Ready cycle may still be running in the worker pool.

**Fix:** Before applying a snapshot, wait for any in-flight apply task to complete.
Add: if `applyInFlight` is true when `!raft.IsEmptySnap(rd.Snapshot)`, block until
`onApplyResult` clears `applyInFlight`. This is not mentioned in any design document.

### Finding 4 (MEDIUM): proposalCallback type conflicts with existing proposalEntry

Section 5.1 defines a new `proposalCallback` struct. The codebase already has
`proposalEntry` (msg.go:192-196) with the same fields plus `proposed time.Time`.
Doc 04 (section 2.2) correctly uses `proposalEntry` in the `Callbacks` map.

**Fix:** Use `proposalEntry` throughout. Remove `proposalCallback` from doc 02.

### Finding 5 (MEDIUM): CompactLog handling is misleading

The `handleReady()` admin loop (peer.go:603-610) processes only ConfChange and
SplitAdmin. CompactLog entries are filtered out of `dataEntries` (peer.go:616) and
effectively become no-ops in `handleReady()`. Actual log truncation is driven by
`onRaftLogGCTick()` -> `scheduleRaftLogGC()`. The `onApplyResult` handler (peer.go:717)
checks for `ExecResultTypeCompactLog` but nothing currently constructs such results in
the normal flow.

The design's `onApplyResult` code (section 7.2) shows handling for CompactLog results.
If the pipeline will route CompactLog through `ApplyResult`, something must construct
those results. Currently nothing does.

**Fix:** Document that CompactLog is handled out-of-band by the GC tick timer. Keep it
excluded from both the data apply path and the admin inline path. The
`ExecResultTypeCompactLog` handler in `onApplyResult` is for future use and is harmless.

### Finding 6 (MEDIUM): Callback thread-safety contract not documented

Section 4.3 moves callback invocation to the apply worker goroutine. The callback
(defined in coordinator.go:148-150) captures the `peer` pointer. `applyEntriesForPeer`
currently only calls `sc.storage.ApplyModifies()` (thread-safe) and does not access
peer state. But if `applyEntriesForPeer` is ever extended to read peer fields (e.g.,
region epoch for stale request rejection), it would need synchronization.

**Fix:** Document the contract: `ApplyFunc` must not read or write any `Peer` fields.
If future changes need peer state inside apply, pass it by value in `ApplyTask`.

### Finding 7 (LOW): Architecture diagram inconsistency

Section 4.2 diagram (line 142) shows "4. Invoke callbacks*" on the peer side, but
section 4.3 says callbacks are invoked in the apply worker. These contradict.

**Fix:** Remove callback invocation from the peer-side diagram.

---

## 03_raft_log_batch_write.md -- NEEDS REVISION (minor)

### Finding 8 (MEDIUM): Hard state in-memory update timing contradiction

Section 7.3 (line 466) calls `p.storage.setHardStateNoLock(rd.HardState)` during
`buildWriteTask()` -- before the data is persisted. Section 9.3 then says "to be safe,
we use the existing `SetHardState()` with a lock." But `SetHardState()` (storage.go:275)
updates the in-memory hard state immediately.

If the peer crashes after `buildWriteTask()` but before `DoneCh` returns, the in-memory
hard state reflects a value that was never persisted. In the current code, `SaveReady()`
(storage.go:222-223) updates `s.hardState` inside the same lock that does `wb.Commit()`.

Doc 04's `ApplyWriteTaskPostPersist` (section 1.2) correctly updates the hard state
AFTER persistence confirmation. This contradicts doc 03's approach.

**Fix:** Follow doc 04's approach exclusively. Do NOT update in-memory hard state in
`buildWriteTask()`. Update it in `ApplyWriteTaskPostPersist()` after `DoneCh` confirms
persistence. Remove the `setHardStateNoLock` call from doc 03 section 7.3.

### Finding 9 (MEDIUM): coalescedCommit partial write on per-task Put error

Section 6 (lines 328-378): if `wb.Put()` fails for task i, the code sends an error on
`DoneCh` and `continue`s. But the failed task's entries that were already Put (if the
hard state succeeded but an entry failed) remain in the WriteBatch. The batch commits
with partial data for that region -- some entries persisted, some not. On restart, the
raft log for that region would have gaps.

Doc 04's version handles this differently: on any Put error, all tasks receive the error
and return early without committing. This is safer (no partial writes) but means one bad
task fails the entire batch.

**Fix:** Either:
- (a) Use `WriteBatch.SetSavePoint()` / `RollbackToSavePoint()` per task (the interface
  supports this at traits.go:94-101). If any Put fails, rollback that task's writes.
- (b) Pre-validate all marshaling in `BuildWriteTask()` (as doc 04 does) so Put never
  fails in the writer. Document this assumption explicitly. This is the simpler approach.

### Finding 10 (LOW): SaveReady line reference off-by-one

Section 2.1 heading says `storage.go:214-258`. Actual function starts at line 215.
The code block correctly shows `line 214` but this appears to be the preceding blank line.

**Fix:** Change section heading to `storage.go:215-258`.

### Finding 11 (LOW): WriteBatch not reused as claimed in 01_overview

Doc 01 (section 4, Optimization 3) claims WriteBatch reuse. The actual implementation
creates a new `WriteBatch` each cycle. The `WriteBatch` interface has `Clear()` at
traits.go:92 that could enable reuse.

**Fix:** Either implement reuse or remove the claim from doc 01. Minor optimization --
pebble batch allocation is cheap.

### Finding 12: Batch write safety -- cross-region ordering -- PASS

Pebble's WriteBatch is atomic. All regions' keys are namespaced by region ID prefix
(via `keys.RaftStateKey` and `keys.RaftLogKey`). No key collisions. No ordering
dependencies between different regions' writes in a single batch. A commit failure
correctly fails all regions. Safe.

---

## 04_migration_plan.md -- NEEDS REVISION

### Finding 13 (HIGH): Shutdown ordering not specified

The coordinator's `Stop()` (coordinator.go:420-448) cancels peer contexts and waits for
`dones` channels. Doc 04 (section 1.4) shows `sc.raftLogWriter.Stop()` in the shutdown
path but does not specify ordering relative to peer shutdown.

Critical issue: if `RaftLogWriter.Stop()` is called BEFORE peers exit, peers mid-
`handleReady` will call `raftLogWriter.Submit()` which blocks on the submission channel.
The writer's goroutine has exited, so `Submit` blocks forever. The peer goroutine never
returns, so `<-sc.dones[regionID]` in `Stop()` blocks forever. **Deadlock.**

If `RaftLogWriter.Stop()` is called AFTER peers exit, it works correctly -- the writer
drains remaining tasks via `drainRemaining()`.

Similarly for `ApplyWorkerPool`: if stopped before peers, the peer may block trying to
send to a stopped pool's channel.

**Fix:** The shutdown order must be:
```
1. Cancel all peer contexts
2. Wait for all peer goroutines to exit (<-dones[regionID])
3. raftLogWriter.Stop()   // drains remaining write tasks
4. applyWorkerPool.Stop() // drains remaining apply tasks
5. Cleanup (router unregistration, etc.)
```

Additionally, `RaftLogWriter.Submit()` should handle the case where the writer has
stopped (check a stopped flag or use a select with stopCh to avoid blocking forever).

### Finding 14 (MEDIUM): Missing applyInFlight gating in code blocks

Doc 04 section 2.2 (`submitToApplyWorker`) does not set `applyInFlight = true` or check
it before submitting. Doc 02 section 11 Step 4 mentions adding this field but the actual
code blocks in doc 04 omit it.

**Fix:** Add `p.applyInFlight = true` at the end of `submitToApplyWorker()`. Add an
early check in the committed entries block:
```go
if p.applyInFlight {
    p.bufferedCommittedEntries = append(p.bufferedCommittedEntries, rd.CommittedEntries...)
    goto advanceRaft
}
```
This connects to Finding 2 -- the buffering strategy must be fully specified.

### Finding 15 (MEDIUM): Admin entry applied index tracking gap

Doc 04 section 2.2 processes admin entries inline before sending data entries to the
apply worker. But it does not update `appliedIndex` for admin entries. If the last
committed entry is admin and there are no data entries, the apply worker receives nothing
and sends no `ApplyResult`. The applied index never updates for this Ready cycle.

**Fix:** After processing admin entries inline, if there are no data entries to submit:
```go
if len(dataEntries) > 0 {
    p.submitToApplyWorker(rd.CommittedEntries)
} else {
    // Only admin entries committed; update applied index inline.
    lastEntry := rd.CommittedEntries[len(rd.CommittedEntries)-1]
    p.storage.SetAppliedIndex(lastEntry.Index)
    p.storage.PersistApplyState()
}
```
This is the same issue as Finding 1 but from the migration plan perspective.

### Finding 16 (MEDIUM): Inconsistent WriteTask definitions between doc 03 and 04

Doc 03 section 5.1 defines `WriteTask` with `HardStateData []byte` and
`Entries []WriteTaskEntry`. Doc 04 section 1.1 redefines it with `Ops []WriteOp`,
`Entries []raftpb.Entry`, and `HardState *raftpb.HardState`. These are incompatible.

Doc 04's version is better because it pre-serializes in `BuildWriteTask` and carries
original data for post-persist in-memory updates.

**Fix:** Declare doc 04's `WriteTask` as canonical. Add a note to doc 03 pointing to
doc 04 for the final structure.

### Finding 17 (MEDIUM): Doc 02/04 have inconsistent ApplyTask field names

Doc 02 uses `ResultMailbox chan<- PeerMsg`. Doc 04 uses `ResultCh chan<- PeerMsg`.

**Fix:** Align field names across both documents.

### Finding 18 (LOW): ApplyWorkerPool.Stop() race with Submit()

After `Stop()` closes `stopCh`, if a goroutine calls `Submit()` before the channel
send blocks, the task may be picked up by the drain loop. But if `Submit()` is called
after all workers have exited, the send blocks forever (channel never read).

**Fix:** Add an `atomic.Bool` stopped flag. `Stop()` sets it before closing `stopCh`.
`Submit()` checks it and returns an error if set.

---

## Cross-Cutting Concerns

### Raft Safety Assessment

**Q: Does propose-apply separation maintain Raft commit guarantees?**
Yes. The raft log is persisted (via SaveReady / RaftLogWriter) BEFORE entries are sent
to the apply worker. Raft's commit guarantee is about log persistence, not state machine
application. If the apply worker crashes, entries are still in the raft log and will be
re-applied on restart (provided `appliedIndex` was not yet persisted past them). The
design correctly persists `ApplyState` only in `onApplyResult()` after worker confirmation.

**Q: Can entries be lost if the apply worker crashes?**
No. Entries are in the persisted raft log. On restart, the node sees
`appliedIndex < committedIndex` and re-applies from the raft log. The only nuance is that
already-applied data may be re-applied (duplicate writes), but these are idempotent
(Put/Delete). See Finding 5b below.

**Q: Does async apply break ReadIndex?**
No, with the pending reads sweep mechanism. ReadIndex assigns a `readIndex` via Raft
quorum confirmation. The read is served only when `appliedIndex >= readIndex`. With async
apply, the read waits longer (until `onApplyResult` advances applied index), but never
serves stale data. Subject to Finding 1 (admin entry applied index must not stall).

**Q: Is cross-region WriteBatch merging safe?**
Yes. See Finding 12.

### Restart / Replay Correctness

On restart with async apply, there is a window where the KV engine has applied data but
`AppliedIndex` on disk is stale. Re-applying entries is safe because all `ApplyModifies`
operations are idempotent (Put and Delete only, verified in `RequestsToModifies` via
`coordinator.go:231` and `storage.go:75-97`). If non-idempotent operations are ever added,
this must be revisited. Consider persisting `ApplyState` atomically with the data write
(same WriteBatch) as a future hardening step.

### Goroutine Lifecycle / Deadlock Analysis

- `RaftLogWriter` channel (256 cap): safe. Backpressure blocks peers, which is correct.
- `ApplyWorkerPool` channel (workers*16 = 64): safe with backpressure.
- `Peer.Mailbox` (256 cap): the apply worker sends `ApplyResult` to this channel. If
  full, the worker blocks, potentially starving other regions. Consider non-blocking send
  with retry/warning, or a separate result channel.
- **Shutdown deadlock** (Finding 13): must stop writers/pools AFTER peers exit.

### Pre-existing Bug: Double callback on same-cycle stepdown

When a leader stepdown (SoftState) and committed entries arrive in the same Ready,
`failAllPendingProposals` (peer.go:563) runs BEFORE committed entries are processed
(line 598+). Proposals are failed with "leader stepped down," then the same proposals
would be moved to `ApplyTask.Callbacks`. However, the design's
`submitToApplyWorker` checks `p.pendingProposals[proposalID]` -- since
`failAllPendingProposals` already deleted them, they won't appear in `Callbacks`. The
apply worker invokes no callback for those entries. The committed data is still applied
(correct), but the client receives only the error callback. This is acceptable behavior
and matches the pre-existing semantics.

---

## Summary of All Findings

| # | Severity | Finding | Affected Doc |
|---|----------|---------|-------------|
| 1 | **HIGH** | Applied index stalls if last committed entry is admin | 02, 04 |
| 2 | **HIGH** | Buffering strategy for committed entries during in-flight apply unspecified | 02, 04 |
| 3 | **HIGH** | Snapshot during in-flight apply causes data corruption | 02 |
| 13 | **HIGH** | Shutdown ordering not specified; deadlock risk | 04 |
| 8 | **MEDIUM** | Hard state in-memory update timing contradiction between doc 03 and 04 | 03 |
| 9 | **MEDIUM** | coalescedCommit partial write on per-task Put error | 03 |
| 14 | **MEDIUM** | Missing applyInFlight gating logic in doc 04 code blocks | 04 |
| 15 | **MEDIUM** | Admin entry applied index tracking gap | 04 |
| 16 | **MEDIUM** | Inconsistent WriteTask definitions between doc 03 and 04 | 03, 04 |
| 17 | **MEDIUM** | Inconsistent ApplyTask field names between doc 02 and 04 | 02, 04 |
| 4 | **MEDIUM** | proposalCallback type conflicts with existing proposalEntry | 02 |
| 5 | **MEDIUM** | CompactLog handling description is misleading | 02, 04 |
| 6 | **MEDIUM** | Callback thread-safety contract not documented | 02 |
| 7 | **LOW** | Architecture diagram callback inconsistency | 02 |
| 10 | **LOW** | Line reference off-by-one in SaveReady | 03 |
| 11 | **LOW** | WriteBatch reuse claimed but not implemented | 01 |
| 18 | **LOW** | ApplyWorkerPool.Stop() race with Submit() | 02, 04 |

### Priority Fixes Before Implementation

1. **Findings 1 + 15**: Ensure applied index tracks the last committed entry (including
   admin entries), not just the last data entry. Handle the admin-only batch case inline.

2. **Finding 2 + 14**: Define the buffering strategy for committed entries when
   `applyInFlight` is true. Specify where buffered entries go, how they are submitted,
   and how applied index is tracked across accumulated batches.

3. **Finding 3**: Add mandatory wait-for-in-flight-apply before snapshot application.
   Add a monotonicity guard to `SetAppliedIndex`.

4. **Finding 13**: Specify shutdown ordering: peers first, then pools/writers.
   Add stopped flag to writer/pool to prevent post-shutdown Submit() deadlock.

5. **Finding 8**: Settle on doc 04's approach for hard state timing. Remove contradictory
   `setHardStateNoLock` from doc 03.

6. **Findings 16 + 17**: Reconcile struct definitions across docs. Declare doc 04 as
   canonical for WriteTask; align ApplyTask field names.

### Missing Test Cases

- TestSnapshotDuringInFlightApply (Finding 3)
- TestAppliedIndexAdvancesForAdminOnlyBatch (Finding 1)
- TestLeaderStepdownDuringApply (pre-existing bug documentation)
- TestCrashBetweenApplyAndPersistState (restart idempotency)
- TestRaftLogWriterPanicRecovery (doc 03 section 8.4)
- TestBatchWriterMultiRegionConcurrent (10+ regions)
- TestApplyPipelineMultiRegionConcurrent (10+ regions)
- TestShutdownOrdering (Finding 13)
