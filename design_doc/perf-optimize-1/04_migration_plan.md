# Performance Optimization Phase 1: Migration Plan

## Table of Contents

1. [Phase 1: Raft Log Batch Writer (Optimizations 2+3)](#1-phase-1-raft-log-batch-writer)
2. [Phase 2: Propose-Apply Pipeline (Optimization 1)](#2-phase-2-propose-apply-pipeline)
3. [Feature Flags](#3-feature-flags)
4. [Rollback Plan](#4-rollback-plan)
5. [Test Checkpoints](#5-test-checkpoints)

---

## 1. Phase 1: Raft Log Batch Writer

This phase introduces a single `RaftLogWriter` goroutine that batches all
regions' raft log persistence into one `WriteBatch` + one fsync per batch cycle.

### Step 1.1: Define WriteTask and RaftLogWriter

**New file:** `internal/raftstore/raft_log_writer.go`

Create the `WriteTask` type that carries a single region's raft log data, and
the `RaftLogWriter` that batches and persists them.

```go
package raftstore

import (
    "log/slog"
    "sync"

    "github.com/ryogrid/gookv/internal/engine/traits"
    "github.com/ryogrid/gookv/pkg/cfnames"
    "github.com/ryogrid/gookv/pkg/keys"
    "go.etcd.io/etcd/raft/v3/raftpb"
)

// WriteOp represents a single key-value write operation.
type WriteOp struct {
    CF    string
    Key   []byte
    Value []byte
}

// WriteTask carries one region's raft log changes for batch persistence.
type WriteTask struct {
    RegionID uint64
    Ops      []WriteOp           // serialized entries + hard state
    Done     chan error           // signaled after persistence completes
    // Post-persist updates (executed by the peer after Done is received).
    Entries        []raftpb.Entry  // for in-memory cache update
    HardState      *raftpb.HardState
}

// RaftLogWriter batches WriteTask submissions from multiple peers into a
// single WriteBatch + fsync per cycle.
type RaftLogWriter struct {
    engine   traits.KvEngine
    taskCh   chan *WriteTask
    stopCh   chan struct{}
    wg       sync.WaitGroup
}

// NewRaftLogWriter creates and starts the writer goroutine.
func NewRaftLogWriter(engine traits.KvEngine, chanSize int) *RaftLogWriter {
    if chanSize <= 0 {
        chanSize = 256
    }
    w := &RaftLogWriter{
        engine: engine,
        taskCh: make(chan *WriteTask, chanSize),
        stopCh: make(chan struct{}),
    }
    w.wg.Add(1)
    go func() {
        defer w.wg.Done()
        w.run()
    }()
    return w
}

// Submit sends a WriteTask to the writer. The caller must wait on task.Done.
func (w *RaftLogWriter) Submit(task *WriteTask) {
    w.taskCh <- task
}

// Stop signals the writer to stop and waits for it to finish.
func (w *RaftLogWriter) Stop() {
    close(w.stopCh)
    w.wg.Wait()
}

func (w *RaftLogWriter) run() {
    for {
        // Block until at least one task arrives or stop is signaled.
        select {
        case task := <-w.taskCh:
            w.processBatch(task)
        case <-w.stopCh:
            // Drain remaining tasks before exiting.
            w.drainRemaining()
            return
        }
    }
}

func (w *RaftLogWriter) processBatch(first *WriteTask) {
    // Collect the first task plus any others already queued.
    tasks := []*WriteTask{first}
    for {
        select {
        case task := <-w.taskCh:
            tasks = append(tasks, task)
        default:
            goto commit
        }
    }

commit:
    // Build a single WriteBatch from all tasks.
    wb := w.engine.NewWriteBatch()
    for _, task := range tasks {
        for _, op := range task.Ops {
            if err := wb.Put(op.CF, op.Key, op.Value); err != nil {
                // Notify all tasks of the error and return.
                for _, t := range tasks {
                    t.Done <- err
                }
                return
            }
        }
    }

    // Single fsync for the entire batch.
    err := wb.Commit()
    if err != nil {
        slog.Error("RaftLogWriter: batch commit failed", "err", err,
            "regions", len(tasks))
    }

    // Notify all waiting peers.
    for _, task := range tasks {
        task.Done <- err
    }
}

func (w *RaftLogWriter) drainRemaining() {
    for {
        select {
        case task := <-w.taskCh:
            w.processBatch(task)
        default:
            return
        }
    }
}
```

**Test checkpoint:** Unit test `RaftLogWriter` in isolation. Submit multiple
`WriteTask`s, verify single batch commit, verify all Done channels receive nil.

### Step 1.2: Add BuildWriteTask to PeerStorage

**File:** `internal/raftstore/storage.go`

Add a method that builds a `WriteTask` from a `raft.Ready` without committing.
This replaces the role of `SaveReady` when the batch writer is enabled.

```go
// BuildWriteTask creates a WriteTask from a Ready without persisting it.
// The caller is responsible for submitting the task to a RaftLogWriter.
func (s *PeerStorage) BuildWriteTask(rd raft.Ready) (*WriteTask, error) {
    s.mu.Lock()
    defer s.mu.Unlock()

    task := &WriteTask{
        RegionID: s.regionID,
        Done:     make(chan error, 1),
    }

    // Serialize hard state.
    if !raft.IsEmptyHardState(rd.HardState) {
        hs := rd.HardState
        task.HardState = &hs
        data, err := hs.Marshal()
        if err != nil {
            return nil, fmt.Errorf("raftstore: marshal hard state: %w", err)
        }
        task.Ops = append(task.Ops, WriteOp{
            CF:    cfnames.CFRaft,
            Key:   keys.RaftStateKey(s.regionID),
            Value: data,
        })
    }

    // Serialize entries.
    for i := range rd.Entries {
        data, err := rd.Entries[i].Marshal()
        if err != nil {
            return nil, fmt.Errorf("raftstore: marshal entry %d: %w",
                rd.Entries[i].Index, err)
        }
        task.Ops = append(task.Ops, WriteOp{
            CF:    cfnames.CFRaft,
            Key:   keys.RaftLogKey(s.regionID, rd.Entries[i].Index),
            Value: data,
        })
    }

    // Store entries for post-persist cache update.
    if len(rd.Entries) > 0 {
        task.Entries = rd.Entries
    }

    return task, nil
}

// ApplyWriteTaskPostPersist updates in-memory state after the WriteTask has
// been persisted by the RaftLogWriter. Must be called by the peer goroutine
// after receiving confirmation from task.Done.
func (s *PeerStorage) ApplyWriteTaskPostPersist(task *WriteTask) {
    s.mu.Lock()
    defer s.mu.Unlock()

    if task.HardState != nil {
        s.hardState = *task.HardState
    }

    if len(task.Entries) > 0 {
        s.appendToCache(task.Entries)
        lastEntry := task.Entries[len(task.Entries)-1]
        if lastEntry.Index > s.persistedLastIndex {
            s.persistedLastIndex = lastEntry.Index
        }
    }
}
```

**Test checkpoint:** Unit test `BuildWriteTask` produces correct `WriteOp`
slice. Compare output against `SaveReady` for the same `raft.Ready`.

### Step 1.3: Change handleReady to Use RaftLogWriter

**File:** `internal/raftstore/peer.go`

Add a `raftLogWriter` field to `Peer`. When set, `handleReady` submits a
`WriteTask` instead of calling `SaveReady`. The peer blocks on `task.Done`
before continuing (preserving current sequential semantics).

Add to `Peer` struct:

```go
type Peer struct {
    // ... existing fields ...
    raftLogWriter *RaftLogWriter // nil = use inline SaveReady (default)
}
```

Add setter:

```go
func (p *Peer) SetRaftLogWriter(w *RaftLogWriter) {
    p.raftLogWriter = w
}
```

Modify `handleReady()` -- replace the `SaveReady` call (lines 578-583):

```go
// Before (current):
//   if err := p.storage.SaveReady(rd); err != nil {
//       return
//   }

// After (with feature flag):
if p.raftLogWriter != nil {
    task, err := p.storage.BuildWriteTask(rd)
    if err != nil {
        return
    }
    p.raftLogWriter.Submit(task)
    // Wait for batch persistence to complete.
    if err := <-task.Done; err != nil {
        return
    }
    // Update in-memory state after successful persistence.
    p.storage.ApplyWriteTaskPostPersist(task)
} else {
    // Legacy path: inline SaveReady.
    if err := p.storage.SaveReady(rd); err != nil {
        return
    }
}
```

**Key invariant preserved:** The peer still blocks until raft logs are persisted
before sending messages or applying entries. The only change is that persistence
may share an fsync with other regions.

### Step 1.4: Wire RaftLogWriter in StoreCoordinator

**File:** `internal/server/coordinator.go`

Create the `RaftLogWriter` once per store and pass it to all peers.

Add to `StoreCoordinator` struct:

```go
type StoreCoordinator struct {
    // ... existing fields ...
    raftLogWriter *raftstore.RaftLogWriter // nil if batch write disabled
}
```

In `NewStoreCoordinator`, conditionally create the writer:

```go
// After engine is set up:
if cfg.EnableBatchRaftWrite {
    sc.raftLogWriter = raftstore.NewRaftLogWriter(cfg.Engine, 256)
}
```

In `BootstrapRegion` and `CreatePeer`, wire the writer to each peer:

```go
// After peer.SetApplyFunc(...):
if sc.raftLogWriter != nil {
    peer.SetRaftLogWriter(sc.raftLogWriter)
}
```

In `Stop`, shut down the writer:

```go
if sc.raftLogWriter != nil {
    sc.raftLogWriter.Stop()
}
```

### Step 1.5: Verification

**Unit tests:**

1. `TestRaftLogWriter_SingleTask` -- one task, verify persisted
2. `TestRaftLogWriter_BatchedTasks` -- submit 10 tasks concurrently, verify all
   persisted in one batch
3. `TestRaftLogWriter_ErrorPropagation` -- engine error propagates to all
   waiters
4. `TestBuildWriteTask_MatchesSaveReady` -- `BuildWriteTask` + manual apply
   produces same engine state as `SaveReady`
5. `TestPeer_HandleReady_WithWriter` -- peer with writer enabled produces same
   committed state as without

**Integration tests:**

```bash
# All existing unit tests must pass
make -f Makefile test

# Existing raftstore tests specifically
go test ./internal/raftstore/... -v -count=1

# Txn integrity demo (multi-region transaction correctness)
# Run with EnableBatchRaftWrite=true in config
./txn-integrity-demo-verify/run.sh
```

**What to check:**
- No data loss: all proposed entries are eventually applied
- No ordering violation: entries applied in index order per region
- No callback leak: all pending proposals eventually receive a callback
- Fsync count: observable via Pebble metrics (expect ~1 fsync per batch cycle
  instead of N per tick)

---

## 2. Phase 2: Propose-Apply Pipeline

This phase decouples entry application from the peer goroutine. Committed
entries are sent to a pool of apply workers via a channel.

### Step 2.1: Create ApplyWorkerPool

**New file:** `internal/raftstore/apply_worker.go`

Reuse the goroutine-pool pattern from `internal/server/flow/flow.go` (`ReadPool`):
a fixed number of workers draining a shared task channel.

```go
package raftstore

import (
    "sync"

    "go.etcd.io/etcd/raft/v3/raftpb"
)

// ApplyTask carries committed entries for async application.
type ApplyTask struct {
    RegionID         uint64
    PeerID           uint64
    Entries          []raftpb.Entry
    // ApplyFunc applies data entries to the KV engine.
    ApplyFunc        func(regionID uint64, entries []raftpb.Entry)
    // Callbacks maps proposalID -> callback for committed entries.
    Callbacks        map[uint64]proposalEntry
    // CurrentTerm is the peer's term at the time entries were committed.
    CurrentTerm      uint64
    // ResultCh receives the ApplyResult after processing completes.
    ResultCh         chan<- PeerMsg
    // RegionID for routing the result back to the peer.
    TargetRegionID   uint64
}

// ApplyWorkerPool processes committed entries asynchronously.
type ApplyWorkerPool struct {
    workers int
    taskCh  chan *ApplyTask
    stopCh  chan struct{}
    wg      sync.WaitGroup
}

// NewApplyWorkerPool creates a pool with the given number of workers.
func NewApplyWorkerPool(workers int) *ApplyWorkerPool {
    if workers <= 0 {
        workers = 4
    }
    pool := &ApplyWorkerPool{
        workers: workers,
        taskCh:  make(chan *ApplyTask, workers*16),
        stopCh:  make(chan struct{}),
    }
    pool.wg.Add(workers)
    for i := 0; i < workers; i++ {
        go func() {
            defer pool.wg.Done()
            pool.worker()
        }()
    }
    return pool
}

// Submit sends an apply task to the pool.
func (p *ApplyWorkerPool) Submit(task *ApplyTask) {
    p.taskCh <- task
}

// Stop signals all workers to stop and waits for them to drain.
func (p *ApplyWorkerPool) Stop() {
    close(p.stopCh)
    p.wg.Wait()
}

func (p *ApplyWorkerPool) worker() {
    for {
        select {
        case task := <-p.taskCh:
            p.processTask(task)
        case <-p.stopCh:
            // Drain remaining.
            for {
                select {
                case task := <-p.taskCh:
                    p.processTask(task)
                default:
                    return
                }
            }
        }
    }
}

func (p *ApplyWorkerPool) processTask(task *ApplyTask) {
    // Apply data entries to KV engine (same as current inline path).
    if task.ApplyFunc != nil {
        task.ApplyFunc(task.RegionID, task.Entries)
    }

    // Build callback results + compute last applied index.
    // (Detailed in Step 2.3)
}
```

**Test checkpoint:** Unit test that `ApplyWorkerPool` processes submitted tasks,
calls `ApplyFunc`, and shuts down cleanly.

### Step 2.2: Change handleReady to Send Entries to Apply Pool

**File:** `internal/raftstore/peer.go`

Add field:

```go
type Peer struct {
    // ... existing fields ...
    applyWorkerPool *ApplyWorkerPool // nil = inline apply (default)
}

func (p *Peer) SetApplyWorkerPool(pool *ApplyWorkerPool) {
    p.applyWorkerPool = pool
}
```

In `handleReady()`, replace the inline apply block (lines 598-653) with
conditional logic:

```go
if len(rd.CommittedEntries) > 0 {
    // Process admin commands inline (same as before -- ConfChange, Split,
    // CompactLog must be applied on the peer goroutine because they modify
    // region metadata).
    for _, e := range rd.CommittedEntries {
        if e.Type == raftpb.EntryConfChange || e.Type == raftpb.EntryConfChangeV2 {
            p.applyConfChangeEntry(e)
        } else if e.Type == raftpb.EntryNormal && IsSplitAdmin(e.Data) {
            eCopy := e
            p.applySplitAdminEntry(&eCopy)
        }
    }

    if p.applyWorkerPool != nil {
        // Async path: send data entries to apply pool.
        p.submitToApplyWorker(rd.CommittedEntries)
    } else {
        // Legacy path: inline apply (current behavior).
        p.applyInline(rd.CommittedEntries)
    }
}
```

The `applyInline` method extracts the current inline logic (filter + applyFunc
+ callbacks + SetAppliedIndex + PersistApplyState) into a separate method.

The `submitToApplyWorker` method builds an `ApplyTask` and submits it:

```go
func (p *Peer) submitToApplyWorker(entries []raftpb.Entry) {
    // Filter to data entries only.
    var dataEntries []raftpb.Entry
    for _, e := range entries {
        if e.Type == raftpb.EntryNormal && !IsSplitAdmin(e.Data) &&
            !IsCompactLog(e.Data) && len(e.Data) > 8 {
            dataEntries = append(dataEntries, e)
        }
    }

    // Extract callbacks for these entries.
    callbacks := make(map[uint64]proposalEntry)
    for _, e := range entries {
        if e.Type != raftpb.EntryNormal || len(e.Data) < 8 {
            continue
        }
        proposalID := binary.BigEndian.Uint64(e.Data[:8])
        if proposalID == 0 {
            continue
        }
        if entry, ok := p.pendingProposals[proposalID]; ok {
            callbacks[proposalID] = entry
            delete(p.pendingProposals, proposalID)
        }
    }

    task := &ApplyTask{
        RegionID:    p.regionID,
        PeerID:      p.peerID,
        Entries:     dataEntries,
        ApplyFunc:   p.applyFunc,
        Callbacks:   callbacks,
        CurrentTerm: p.currentTerm,
        ResultCh:    p.Mailbox,
        TargetRegionID: p.regionID,
    }

    p.applyWorkerPool.Submit(task)

    // Note: DO NOT update applied index or persist apply state here.
    // The apply worker will do it and send an ApplyResult back.
}
```

**Critical design decision:** Admin entries (ConfChange, Split, CompactLog) are
still applied inline on the peer goroutine. Only data entries are offloaded.
This preserves the invariant that region metadata changes happen before data
application.

### Step 2.3: Move Callback Invocation to Apply Worker

**File:** `internal/raftstore/apply_worker.go`

Expand `processTask` to invoke callbacks and compute the last applied index:

```go
func (p *ApplyWorkerPool) processTask(task *ApplyTask) {
    // Apply data entries to KV engine.
    if task.ApplyFunc != nil && len(task.Entries) > 0 {
        task.ApplyFunc(task.RegionID, task.Entries)
    }

    // Invoke proposal callbacks (same logic as current inline path).
    for _, e := range task.Entries {
        if len(e.Data) < 8 {
            continue
        }
        proposalID := binary.BigEndian.Uint64(e.Data[:8])
        if proposalID == 0 {
            continue
        }
        if entry, ok := task.Callbacks[proposalID]; ok {
            if e.Term == entry.term {
                entry.callback(nil) // success
            } else {
                entry.callback(errorResponse(
                    fmt.Errorf("term mismatch: proposed in %d, committed in %d",
                        entry.term, e.Term)))
            }
        }
    }

    // Compute last applied index from the full committed entries batch.
    var lastAppliedIndex uint64
    if len(task.Entries) > 0 {
        lastAppliedIndex = task.Entries[len(task.Entries)-1].Index
    }

    // Send result back to peer via its mailbox.
    if task.ResultCh != nil {
        task.ResultCh <- PeerMsg{
            Type: PeerMsgTypeApplyResult,
            Data: &ApplyResult{
                RegionID:     task.RegionID,
                AppliedIndex: lastAppliedIndex,
            },
        }
    }
}
```

**Note:** This requires adding an `AppliedIndex` field to the existing
`ApplyResult` struct in `internal/raftstore/msg.go`:

```go
type ApplyResult struct {
    RegionID     uint64
    AppliedIndex uint64       // NEW: last applied index from this batch
    Results      []ExecResult
}
```

### Step 2.4: Coordinate Applied Index Between Peer and Apply Worker

**File:** `internal/raftstore/peer.go`

Update `onApplyResult` (line 710) to handle the applied index from the async
apply path:

```go
func (p *Peer) onApplyResult(result *ApplyResult) {
    if result == nil {
        return
    }

    // Update applied index from async apply worker.
    if result.AppliedIndex > 0 {
        currentApplied := p.storage.AppliedIndex()
        if result.AppliedIndex > currentApplied {
            p.storage.SetAppliedIndex(result.AppliedIndex)

            // Persist apply state (same as current inline path).
            if err := p.storage.PersistApplyState(); err != nil {
                slog.Error("APPLY-STATE-PERSIST failed",
                    "error", err, "region", p.regionID)
            }

            // Sweep pending reads that may now be satisfiable.
            if len(p.pendingReads) > 0 {
                for key, pr := range p.pendingReads {
                    if pr.readIndex > 0 && result.AppliedIndex >= pr.readIndex {
                        pr.callback(nil)
                        delete(p.pendingReads, key)
                    }
                }
            }
        }
    }

    // Process exec results (CompactLog, etc.) -- existing logic.
    for _, r := range result.Results {
        switch r.Type {
        case ExecResultTypeCompactLog:
            if clr, ok := r.Data.(*CompactLogResult); ok {
                p.onReadyCompactLog(*clr)
            }
        }
    }
}
```

### Step 2.5: Update ReadIndex to Wait for Async Applied Index

**File:** `internal/raftstore/peer.go`

The current ReadIndex logic (lines 662-684 in `handleReady`) checks
`storage.AppliedIndex()` immediately after applying entries inline. With async
apply, the applied index may not yet reflect the committed entries.

The fix is already in place: when `handleReady` uses the async path, it does
NOT update the applied index itself. Instead, `onApplyResult` updates it and
sweeps pending reads. The existing `pendingReads` mechanism already handles the
case where `appliedIdx < rs.Index` -- the read stays pending until a future
sweep satisfies it.

However, we must ensure the sweep in `handleReady` (lines 676-684) is still
correct. With async apply, the applied index may advance via `onApplyResult`
between two `handleReady` calls. The existing sweep logic at lines 676-684
already handles this because it reads `storage.AppliedIndex()` fresh each time.

**No code change needed here** -- the existing mechanisms handle the async case
correctly. The key insight is:

1. `handleReady` processes `ReadStates` and stores them in `pendingReads` with
   the required `readIndex`
2. If `appliedIdx >= readIndex`, the read is satisfied immediately
3. If not, the read stays pending
4. `onApplyResult` advances `appliedIdx` and sweeps pending reads
5. Future `handleReady` calls also sweep pending reads

### Step 2.6: Wire ApplyWorkerPool in StoreCoordinator

**File:** `internal/server/coordinator.go`

Add to `StoreCoordinator` struct:

```go
type StoreCoordinator struct {
    // ... existing fields ...
    applyWorkerPool *raftstore.ApplyWorkerPool // nil if pipeline disabled
}
```

In `NewStoreCoordinator`:

```go
if cfg.EnableApplyPipeline {
    sc.applyWorkerPool = raftstore.NewApplyWorkerPool(4)
}
```

In `BootstrapRegion` and `CreatePeer`:

```go
if sc.applyWorkerPool != nil {
    peer.SetApplyWorkerPool(sc.applyWorkerPool)
}
```

In `Stop`:

```go
if sc.applyWorkerPool != nil {
    sc.applyWorkerPool.Stop()
}
```

### Step 2.7: Verification

**Unit tests:**

1. `TestApplyWorkerPool_ProcessTask` -- entries applied, callbacks invoked
2. `TestApplyWorkerPool_AppliedIndexResult` -- correct applied index in result
3. `TestApplyWorkerPool_TermMismatch` -- callback receives error on term mismatch
4. `TestApplyWorkerPool_Drain` -- pending tasks processed on Stop
5. `TestPeer_AsyncApply_ReadIndex` -- ReadIndex satisfied after async apply result
6. `TestPeer_AsyncApply_AdminInline` -- admin entries still applied inline

**Integration tests:**

```bash
# All existing unit tests
make -f Makefile test

# Raftstore tests (critical for correctness)
go test ./internal/raftstore/... -v -count=1

# Coordinator tests
go test ./internal/server/... -v -count=1

# Txn integrity demo with pipeline enabled
./txn-integrity-demo-verify/run.sh

# Restart recovery test: start cluster, write data, kill nodes,
# restart, verify data integrity
```

**What to check:**
- All proposal callbacks fire exactly once
- Applied index is monotonically non-decreasing per region
- ReadIndex returns only after applied index reaches read index
- No stale reads: ReadIndex after a write must see the write
- Admin entries (ConfChange, Split) still apply synchronously
- PersistApplyState called after every apply batch (restart safety)

---

## 3. Feature Flags

Both optimizations are controlled by configuration flags, allowing independent
enable/disable at startup.

**File:** `internal/server/coordinator.go` (in `StoreCoordinatorConfig`):

```go
type StoreCoordinatorConfig struct {
    // ... existing fields ...

    // EnableBatchRaftWrite enables the RaftLogWriter that batches multiple
    // regions' raft log writes into a single fsync. Default: false.
    EnableBatchRaftWrite bool

    // EnableApplyPipeline enables the async apply worker pool that decouples
    // entry application from the peer goroutine. Default: false.
    EnableApplyPipeline bool
}
```

**Behavior matrix:**

| BatchRaftWrite | ApplyPipeline | Behavior |
|---|---|---|
| false | false | Current behavior (no change) |
| true | false | Batched raft log writes, inline apply |
| false | true | Inline raft log writes, async apply |
| true | true | Both optimizations active |

Each flag independently controls its optimization. There are no ordering
constraints between the two flags.

**How the flags flow:**

1. Config is read at startup (from config file or defaults)
2. `NewStoreCoordinator` checks flags and conditionally creates
   `RaftLogWriter` and/or `ApplyWorkerPool`
3. When creating peers (`BootstrapRegion`, `CreatePeer`), the coordinator
   sets `peer.raftLogWriter` and/or `peer.applyWorkerPool` if non-nil
4. In `handleReady`, the peer checks these fields to decide which path to take

---

## 4. Rollback Plan

### Phase 1 (Batch Writer) Rollback

If tests fail after implementing Phase 1:

1. Set `EnableBatchRaftWrite = false` in config
2. This causes `peer.raftLogWriter` to be nil for all peers
3. `handleReady` falls through to the `else` branch and calls `SaveReady`
   directly (the original code path, which is not modified or removed)
4. No data migration needed -- the on-disk format is identical

If the issue is in `RaftLogWriter` itself:

1. The `SaveReady` method is preserved unchanged
2. The `BuildWriteTask` and `ApplyWriteTaskPostPersist` methods are additive
   (no existing code is modified)
3. Reverting = deleting `raft_log_writer.go`, removing the flag, and removing
   the conditional in `handleReady`

### Phase 2 (Apply Pipeline) Rollback

If tests fail after implementing Phase 2:

1. Set `EnableApplyPipeline = false` in config
2. `peer.applyWorkerPool` is nil for all peers
3. `handleReady` calls `applyInline` (the refactored-but-equivalent original
   code path)

If the issue is in `ApplyWorkerPool`:

1. The `applyInline` method preserves the exact current inline behavior
2. `onApplyResult` changes are backward-compatible (the new `AppliedIndex`
   field defaults to 0, which is ignored)
3. Reverting = deleting `apply_worker.go`, removing the flag, and collapsing
   `applyInline` back into `handleReady`

### Emergency Procedure

If a production issue is suspected:

1. Stop the node
2. Set both flags to `false`
3. Restart -- the node operates exactly as before the optimization
4. No data corruption is possible because the on-disk format does not change

---

## 5. Test Checkpoints

Summary of verification gates at each step. All gates must pass before
proceeding to the next step.

### Phase 1 Checkpoints

| Step | Gate | Command |
|---|---|---|
| 1.1 | `RaftLogWriter` unit tests pass | `go test ./internal/raftstore/ -run TestRaftLogWriter -v` |
| 1.2 | `BuildWriteTask` unit tests pass | `go test ./internal/raftstore/ -run TestBuildWriteTask -v` |
| 1.3 | All existing raftstore tests pass with writer enabled | `go test ./internal/raftstore/... -v -count=1` |
| 1.4 | Full test suite passes | `make -f Makefile test` |
| 1.5 | txn-integrity-demo passes with `EnableBatchRaftWrite=true` | Manual verification |

### Phase 2 Checkpoints

| Step | Gate | Command |
|---|---|---|
| 2.1 | `ApplyWorkerPool` unit tests pass | `go test ./internal/raftstore/ -run TestApplyWorker -v` |
| 2.2 | Existing tests pass with pipeline disabled (refactor only) | `go test ./internal/raftstore/... -v -count=1` |
| 2.3 | Callback invocation tests pass | `go test ./internal/raftstore/ -run TestApplyWorker_Callback -v` |
| 2.4 | Applied index coordination tests pass | `go test ./internal/raftstore/ -run TestPeer_AsyncApply -v` |
| 2.5 | ReadIndex tests pass with pipeline enabled | `go test ./internal/raftstore/ -run TestReadIndex -v` |
| 2.6 | Full test suite passes | `make -f Makefile test` |
| 2.7 | txn-integrity-demo passes with `EnableApplyPipeline=true` | Manual verification |

### Combined Checkpoint

After both phases are implemented:

| Gate | Verification |
|---|---|
| Both flags enabled, full test suite | `make -f Makefile test` with both flags true |
| txn-integrity-demo with both flags | Manual: concurrent multi-region transactions |
| Restart recovery with both flags | Kill + restart, verify no data loss |
| Fallback: disable both, run tests | `make -f Makefile test` with both flags false |

---

## Appendix: File Change Summary

### New Files

| File | Phase | Purpose |
|---|---|---|
| `internal/raftstore/raft_log_writer.go` | 1 | `RaftLogWriter`, `WriteTask`, `WriteOp` |
| `internal/raftstore/raft_log_writer_test.go` | 1 | Unit tests for batch writer |
| `internal/raftstore/apply_worker.go` | 2 | `ApplyWorkerPool`, `ApplyTask` |
| `internal/raftstore/apply_worker_test.go` | 2 | Unit tests for apply pool |

### Modified Files

| File | Phase | Changes |
|---|---|---|
| `internal/raftstore/storage.go` | 1 | Add `BuildWriteTask`, `ApplyWriteTaskPostPersist` |
| `internal/raftstore/peer.go` | 1 | Add `raftLogWriter` field, conditional in `handleReady` |
| `internal/raftstore/peer.go` | 2 | Add `applyWorkerPool` field, `submitToApplyWorker`, `applyInline`, update `onApplyResult` |
| `internal/raftstore/msg.go` | 2 | Add `AppliedIndex` field to `ApplyResult` |
| `internal/server/coordinator.go` | 1+2 | Add writer/pool fields, wire to peers, config flags |

### Unchanged Files

| File | Reason |
|---|---|
| `internal/raftstore/storage.go` (`SaveReady`) | Preserved as legacy path |
| `internal/server/flow/flow.go` | Reference only, not modified |
| `internal/engine/traits/traits.go` | Engine interface unchanged |
| `internal/server/storage.go` (`ApplyModifies`) | Called by apply worker same as before |

---

## Addendum: Review Feedback Incorporated

This section addresses findings from the design review (2026-03-28 v2) that
affect this document. Existing content above is unchanged; the corrections
and clarifications below take precedence where they conflict.

### A1. Shutdown Ordering -- Deadlock Prevention (Finding 13 -- HIGH)

**Problem:** Section 1.4 shows `sc.raftLogWriter.Stop()` in the shutdown path
but does not specify ordering relative to peer shutdown. If `RaftLogWriter.Stop()`
is called BEFORE peers exit, peers mid-`handleReady` will call
`raftLogWriter.Submit()` which blocks on the submission channel. The writer's
goroutine has exited, so `Submit` blocks forever. The peer goroutine never
returns, so `<-sc.dones[regionID]` in `Stop()` blocks forever. **Deadlock.**

The same problem applies to `ApplyWorkerPool.Stop()`.

**Fix:** The shutdown sequence in `StoreCoordinator.Stop()` must be:

```go
func (sc *StoreCoordinator) Stop() {
    // Step 1: Cancel all peer contexts.
    // This signals each peer's Run() loop to exit.
    for _, cancel := range sc.cancels {
        cancel()
    }

    // Step 2: Wait for ALL peer goroutines to exit.
    // Peers may still be mid-handleReady; they must finish or abort
    // before we tear down the infrastructure they depend on.
    for regionID := range sc.dones {
        <-sc.dones[regionID]
    }

    // Step 3: Stop the RaftLogWriter.
    // Safe: no peers are running, so no new Submit() calls.
    // drainRemaining() will process any tasks left in the channel.
    if sc.raftLogWriter != nil {
        sc.raftLogWriter.Stop()
    }

    // Step 4: Stop the ApplyWorkerPool.
    // Safe: no peers are running, so no new Submit() calls.
    // Worker drain will process remaining apply tasks.
    if sc.applyWorkerPool != nil {
        sc.applyWorkerPool.Stop()
    }

    // Step 5: Cleanup (router unregistration, etc.)
    sc.router.UnregisterAll()
}
```

Additionally, both `RaftLogWriter.Submit()` and `ApplyWorkerPool.Submit()`
should include a stopped flag check to prevent indefinite blocking if called
after shutdown begins:

```go
// RaftLogWriter
func (w *RaftLogWriter) Submit(task *WriteTask) error {
    if w.stopped.Load() {
        return fmt.Errorf("raftlogwriter: writer is stopped")
    }
    w.taskCh <- task
    return nil
}

// ApplyWorkerPool
func (p *ApplyWorkerPool) Submit(task *ApplyTask) error {
    if p.stopped.Load() {
        return fmt.Errorf("apply worker pool is stopped")
    }
    p.taskCh <- task
    return nil
}
```

In both cases, `Stop()` sets the `stopped` flag (`atomic.Bool`) before
closing `stopCh`.

### A2. Applied Index Stalls on Admin-Only Batches (Findings 1 + 15 -- HIGH/MEDIUM)

**Problem:** Section 2.2 (`submitToApplyWorker`) processes admin entries inline
before sending data entries to the apply worker. But it does not update
`appliedIndex` for admin entries. If the last committed entry is admin and there
are no data entries, the apply worker receives nothing and sends no
`ApplyResult`. The applied index never updates for this Ready cycle.

**Fix:** Add a `LastCommittedIndex uint64` field to `ApplyTask` (see doc 02
addendum A1 for the struct change). After processing admin entries inline,
check if there are data entries to submit:

```go
lastCommittedIndex := rd.CommittedEntries[len(rd.CommittedEntries)-1].Index

if len(dataEntries) > 0 {
    task := &ApplyTask{
        RegionID:           p.regionID,
        Entries:            dataEntries,
        LastCommittedIndex: lastCommittedIndex,
        // ... other fields ...
    }
    p.applyInFlight = true
    p.applyWorkerPool.Submit(task)
} else {
    // Only admin entries committed; update applied index inline.
    // No apply task is submitted, so no ApplyResult will arrive.
    p.storage.SetAppliedIndex(lastCommittedIndex)
    if err := p.storage.PersistApplyState(); err != nil {
        slog.Error("APPLY-STATE-PERSIST failed",
            "error", err, "region", p.regionID)
    }

    // Sweep pending reads that may now be satisfiable.
    appliedIdx := p.storage.AppliedIndex()
    for key, pr := range p.pendingReads {
        if pr.readIndex > 0 && appliedIdx >= pr.readIndex {
            pr.callback(nil)
            delete(p.pendingReads, key)
        }
    }
}
```

In `onApplyResult`, use the `LastCommittedIndex` from the result:

```go
if result.AppliedIndex > 0 {
    currentApplied := p.storage.AppliedIndex()
    // Monotonicity guard: never regress the applied index.
    if result.AppliedIndex > currentApplied {
        p.storage.SetAppliedIndex(result.AppliedIndex)
        // ...
    }
}
```

### A3. Missing applyInFlight Gating (Finding 14 -- MEDIUM)

**Problem:** Section 2.2 (`submitToApplyWorker`) does not set
`applyInFlight = true` or check it before submitting.

**Fix:** Add `p.applyInFlight = true` at the end of `submitToApplyWorker()`.
Add an early check in the committed entries block when `applyInFlight` is
true to buffer new entries (see doc 02 addendum A2 for the buffering strategy):

```go
// At the start of the committed entries block in handleReady():
if p.applyInFlight && p.applyWorkerPool != nil {
    // Previous apply is still in-flight. Buffer this batch.
    task := buildApplyTask(rd.CommittedEntries, lastCommittedIndex, ...)
    p.pendingApplyTasks = append(p.pendingApplyTasks, task)
    goto advanceRaft
}
```

Admin entries are still processed inline even when buffering, because they
modify region metadata that must be applied before any subsequent data entries.

### A4. Struct Consistency -- WriteTask (Finding 16 -- MEDIUM)

**Problem:** Doc 03 section 5.1 defines `WriteTask` with `HardStateData []byte`
and `Entries []WriteTaskEntry`. This document's section 1.1 defines `WriteTask`
with `Ops []WriteOp`, `Entries []raftpb.Entry`, and
`HardState *raftpb.HardState`.

**Resolution:** The `WriteTask` definition in this document (section 1.1) is
canonical. It pre-serializes entries into `[]WriteOp` for the writer while
carrying original data for post-persist in-memory updates via
`ApplyWriteTaskPostPersist`. Doc 03's `WriteTask` and `WriteTaskEntry` structs
are superseded.

### A5. Struct Consistency -- ApplyTask Field Names (Finding 17 -- MEDIUM)

**Problem:** Doc 02 uses `ResultMailbox chan<- PeerMsg` while this document
uses `ResultCh chan<- PeerMsg`.

**Resolution:** The canonical field name is `ResultCh` (as used in this
document). Doc 02's `ResultMailbox` references should be read as `ResultCh`.

### A6. Additional Test Scenarios (Findings 1, 2, 3, 8, 13)

The following test cases must be added to the Phase 2 checkpoints (section 5)
to cover the issues identified in the review:

| Test Case | Finding | Description |
|---|---|---|
| `TestSnapshotDuringInFlightApply` | 3 | Submit an apply task, then deliver a snapshot Ready while the apply is in-flight. Verify the peer waits for the in-flight apply to complete before applying the snapshot. Verify no data corruption in the key range. |
| `TestAppliedIndexAdvancesForAdminOnlyBatch` | 1, 15 | Commit a batch where the last entry is a CompactLog or SplitAdmin (no data entries). Verify `appliedIndex` advances to the last committed entry's index, not zero. Verify pending ReadIndex requests are unblocked. |
| `TestLeaderStepdownDuringApply` | Pre-existing | Trigger a leader stepdown (SoftState change) in the same Ready that contains committed entries. Verify `failAllPendingProposals` runs, the apply worker still applies the committed data, and no double-callback occurs. |
| `TestCrashRecoveryIdempotency` | Cross-cutting | Apply entries via the async path, then simulate a crash (kill the peer) before `PersistApplyState` in `onApplyResult`. On restart, verify the node re-applies from the persisted `appliedIndex` and all data is consistent. Verify Put/Delete idempotency. |
| `TestRaftLogWriterPanicRecovery` | 8.4, A2 in doc 03 | Inject a panic in the writer goroutine (e.g., via a test hook). Verify all pending tasks receive an error on `DoneCh`. Verify no peer blocks forever. |
| `TestShutdownOrdering` | 13 | Start a store with multiple active regions. Initiate shutdown while peers are mid-`handleReady`. Verify no deadlock: all peer goroutines exit, then the writer drains, then the apply pool drains. Verify graceful completion within a timeout. |

These should be added to both the Phase 2 unit test checkpoint table and the
Combined Checkpoint section.
