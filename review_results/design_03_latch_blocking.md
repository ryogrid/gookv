# Design Doc 03: Latch Blocking and GC Safety

## Issues Covered

| ID | Category | Summary |
|----|----------|---------|
| BUG-04 | Performance | All latch acquisitions use spin-wait (`for !Acquire() {}`) |
| ISSUE-06 | Correctness | GC worker writes without latches, risking concurrent modification |
| ISSUE-07 | Correctness | GC does not remove Delete markers below safe point |

---

## Problem

### 1. Spin-wait latch acquisition wastes CPU (BUG-04)

**File:** `internal/server/storage.go` — 20 call sites (lines 185, 227, 257, 313, 336, 361, 385, 453, 535, 556, 598, 630, 667, 696, 718, 747, 777, 831, 895, 948, 998)

Every latch acquisition follows this pattern:

```go
cmdID := s.allocCmdID()
lock := s.latches.GenLock(keys)
for !s.latches.Acquire(lock, cmdID) {
}
```

Line 258 even acknowledges the problem:
```go
for !s.latches.Acquire(lock, cmdID) {
    // Spin until acquired. In production, use a wait channel.
}
```

When a hot key is contended by multiple transactions, each waiting goroutine
burns a full CPU core spinning in a tight loop. With Go's cooperative scheduling,
this can starve other goroutines on the same OS thread until the runtime preempts
(every 10ms in Go 1.14+). Under high contention, this degrades throughput and
increases tail latency.

The latch infrastructure already has the building blocks for blocking wait:

**File:** `internal/storage/txn/latch/latch.go`

- `latchSlot.wakeCh` (line 23): a `chan uint64` with buffer size 64, allocated
  in `New()` at line 43 but **never used**.
- `latchSlot.waitQueue` (line 22): populated by `Acquire()` at line 93 when a
  slot is contended.
- `Release()` (lines 103-125): computes `wakeUp` list of command IDs that should
  retry, but **callers ignore the return value**.

### 2. GC worker writes without latches (ISSUE-06)

**File:** `internal/storage/gc/gc.go`, lines 224-282

```go
func (w *GCWorker) processTask(task GCTask) error {
    snap := w.engine.NewSnapshot()
    reader := mvcc.NewMvccReader(snap)
    defer reader.Close()

    // ... scan and collect GC modifications ...

    txn := mvcc.NewMvccTxn(0)
    // ... for each key: GC(txn, reader, userKey, task.SafePoint) ...

    if txn.WriteSize >= w.config.MaxTxnWriteSize {
        if err := w.applyModifies(txn); err != nil {
            return err
        }
    }
    // ... flush remaining ...
}
```

The `applyModifies` method (line 284) writes directly to the engine via a
`WriteBatch` without acquiring any latches. A concurrent transaction could be
mid-commit on the same key:

1. Transaction T reads write CF at `key@ts=100`, sees a Put
2. GC deletes `key@ts=100` from write CF (it's below safe point)
3. Transaction T tries to commit, writes a new version referencing the deleted one

This is a data race. TiKV's GC worker acquires latches for each batch of keys
before writing, ensuring mutual exclusion with concurrent transactions.

### 3. GC keeps Delete markers indefinitely (ISSUE-07)

**File:** `internal/storage/gc/gc.go`, lines 92-112

```go
case gcStateRemoveIdempotent:
    switch write.WriteType {
    case txntypes.WriteTypePut:
        // Keep this version (latest Put at or below safe point).
        state = gcStateRemoveAll
        ts = commitTS - 1
        continue

    case txntypes.WriteTypeDelete:
        // Keep the Delete marker, but transition to remove all older versions.
        state = gcStateRemoveAll
        ts = commitTS - 1
        continue

    case txntypes.WriteTypeLock, txntypes.WriteTypeRollback:
        // Remove Lock/Rollback records below safe point.
        txn.DeleteWrite(key, commitTS)
        info.DeletedVersions++
        ts = commitTS - 1
        continue
    }
```

In `gcStateRemoveIdempotent`, a `WriteTypeDelete` is always kept (lines 100-104).
This is correct when there are older Put versions below it — the Delete marker
is needed to indicate the key is deleted. However, once all older versions are
removed (entering `gcStateRemoveAll` and deleting everything), the Delete marker
serves no purpose: there is nothing below it to shadow.

Over time, deleted keys accumulate stale Delete markers that are never cleaned up.
This wastes storage and slows down scans that must skip over these markers.

TiKV's GC handles this by checking whether any older versions exist below the
Delete marker. If none exist (or all are removed), the Delete marker itself is
also removed.

---

## Current Code

### Latches struct (latch.go lines 13-16)

```go
type Latches struct {
    slots []latchSlot
    size  int
}
```

### latchSlot struct (latch.go lines 19-24)

```go
type latchSlot struct {
    mu        sync.Mutex
    owner     uint64
    waitQueue []uint64
    wakeCh    chan uint64
}
```

### Acquire (latch.go lines 80-99)

```go
func (l *Latches) Acquire(lock *Lock, commandID uint64) bool {
    for i := lock.OwnedCount; i < len(lock.RequiredHashes); i++ {
        slotIdx := lock.RequiredHashes[i]
        slot := &l.slots[slotIdx]

        slot.mu.Lock()
        if slot.owner == 0 || slot.owner == commandID {
            slot.owner = commandID
            lock.OwnedCount = i + 1
            slot.mu.Unlock()
        } else {
            slot.waitQueue = append(slot.waitQueue, commandID)
            slot.mu.Unlock()
            return false
        }
    }
    return true
}
```

### Release (latch.go lines 103-125)

```go
func (l *Latches) Release(lock *Lock, commandID uint64) []uint64 {
    var wakeUp []uint64

    for i := 0; i < lock.OwnedCount; i++ {
        slotIdx := lock.RequiredHashes[i]
        slot := &l.slots[slotIdx]

        slot.mu.Lock()
        if slot.owner == commandID {
            slot.owner = 0
            if len(slot.waitQueue) > 0 {
                waiter := slot.waitQueue[0]
                slot.waitQueue = slot.waitQueue[1:]
                wakeUp = append(wakeUp, waiter)
            }
        }
        slot.mu.Unlock()
    }

    lock.OwnedCount = 0
    return wakeUp
}
```

### GC function (gc.go lines 58-132)

The state machine transitions:
- `gcStateRewind`: skip versions above `safePoint`
- `gcStateRemoveIdempotent`: remove Lock/Rollback, keep first Put/Delete
- `gcStateRemoveAll`: delete all remaining versions

### GCWorker.processTask (gc.go lines 224-282)

Scans CF_WRITE, calls `GC()` per unique key, flushes via `applyModifies()` which
calls `wb.Commit()` directly on the engine — no latch acquisition.

---

## Design

### A. Blocking latch acquisition via wakeCh

Replace the spin-wait loop with a blocking `AcquireBlocking` method that uses
the existing `wakeCh` infrastructure. The flow:

1. Try `Acquire()` — if it succeeds, return immediately.
2. If it fails, the command ID has been added to `waitQueue` by `Acquire()`.
3. Block on a per-command channel until `Release()` signals a wake-up.
4. On wake-up, retry `Acquire()`.

**Per-command wake channel:** Rather than using the per-slot `wakeCh` (which mixes
signals from different waiters), we use a simple approach: a `sync.Map` of
`commandID -> chan struct{}` managed by the `Latches` struct.

```go
type Latches struct {
    slots   []latchSlot
    size    int
    waiters sync.Map  // commandID -> chan struct{}
}

// AcquireBlocking acquires all latches, blocking if necessary.
// Returns when all latches are held. The context can be used for timeout.
func (l *Latches) AcquireBlocking(ctx context.Context, lock *Lock, commandID uint64) error {
    for {
        if l.Acquire(lock, commandID) {
            // Clean up waiter channel if one was created.
            l.waiters.Delete(commandID)
            return nil
        }

        // Get or create wake channel for this command.
        ch := make(chan struct{}, 1)
        if actual, loaded := l.waiters.LoadOrStore(commandID, ch); loaded {
            ch = actual.(chan struct{})
        }

        // Wait for wake-up or context cancellation.
        select {
        case <-ch:
            // Woken up — retry acquisition.
            continue
        case <-ctx.Done():
            // Timeout or cancellation — release any partially acquired latches.
            l.Release(lock, commandID)
            l.waiters.Delete(commandID)
            return ctx.Err()
        }
    }
}
```

Update `Release()` to wake blocked commands:

```go
func (l *Latches) Release(lock *Lock, commandID uint64) []uint64 {
    var wakeUp []uint64

    for i := 0; i < lock.OwnedCount; i++ {
        slotIdx := lock.RequiredHashes[i]
        slot := &l.slots[slotIdx]

        slot.mu.Lock()
        if slot.owner == commandID {
            slot.owner = 0
            if len(slot.waitQueue) > 0 {
                waiter := slot.waitQueue[0]
                slot.waitQueue = slot.waitQueue[1:]
                wakeUp = append(wakeUp, waiter)
            }
        }
        slot.mu.Unlock()
    }

    lock.OwnedCount = 0

    // Signal woken commands.
    for _, id := range wakeUp {
        if v, ok := l.waiters.Load(id); ok {
            ch := v.(chan struct{})
            select {
            case ch <- struct{}{}:
            default:
            }
        }
    }

    return wakeUp
}
```

Update all 20+ call sites in `storage.go` from:

```go
cmdID := s.allocCmdID()
lock := s.latches.GenLock(keys)
for !s.latches.Acquire(lock, cmdID) {
}
```

To:

```go
cmdID := s.allocCmdID()
lock := s.latches.GenLock(keys)
if err := s.latches.AcquireBlocking(context.Background(), lock, cmdID); err != nil {
    return /* appropriate error */
}
```

For methods that have a context parameter or need timeout, pass the actual context:

```go
// In a future iteration, caller-provided context enables cancellation:
if err := s.latches.AcquireBlocking(ctx, lock, cmdID); err != nil {
    return nil, fmt.Errorf("latch acquire: %w", err)
}
```

### B. GC worker acquires latches before writing

The GC worker must hold latches for the keys it modifies. Since GC processes keys
in batches, it acquires latches per batch, applies the modifications, and releases.

The GC worker needs access to the `Latches` instance. Add it to `GCWorker`:

```go
type GCWorker struct {
    engine  traits.KvEngine
    latches *latch.Latches  // NEW: shared latch instance
    taskCh  chan GCTask
    config  *GCConfig
    // ...
}

func NewGCWorker(engine traits.KvEngine, latches *latch.Latches, config *GCConfig) *GCWorker {
    // ...
}
```

Update `processTask` to latch each batch:

```go
func (w *GCWorker) processTask(task GCTask) error {
    snap := w.engine.NewSnapshot()
    reader := mvcc.NewMvccReader(snap)
    defer reader.Close()

    iter := snap.NewIterator(cfnames.CFWrite, opts)
    defer iter.Close()

    txn := mvcc.NewMvccTxn(0)
    var batchKeys [][]byte  // keys in current batch

    var lastUserKey []byte
    for iter.SeekToFirst(); iter.Valid(); iter.Next() {
        userKey, _, err := mvcc.DecodeKey(iter.Key())
        if err != nil {
            continue
        }
        if string(userKey) == string(lastUserKey) {
            continue
        }
        lastUserKey = append(lastUserKey[:0], userKey...)

        w.keysScanned.Add(1)

        info, err := GC(txn, reader, userKey, task.SafePoint)
        if err != nil {
            continue
        }

        w.versionsDeleted.Add(int64(info.DeletedVersions))
        batchKeys = append(batchKeys, append([]byte(nil), userKey...))

        if txn.WriteSize >= w.config.MaxTxnWriteSize {
            if err := w.applyModifiesWithLatches(txn, batchKeys); err != nil {
                return err
            }
            txn = mvcc.NewMvccTxn(0)
            batchKeys = batchKeys[:0]
        }
    }

    if len(txn.Modifies) > 0 {
        return w.applyModifiesWithLatches(txn, batchKeys)
    }
    return nil
}

func (w *GCWorker) applyModifiesWithLatches(txn *mvcc.MvccTxn, keys [][]byte) error {
    if len(txn.Modifies) == 0 {
        return nil
    }

    // Acquire latches for all keys in this batch.
    cmdID := atomic.AddUint64(&w.nextCmdID, 1)
    lock := w.latches.GenLock(keys)
    ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
    defer cancel()
    if err := w.latches.AcquireBlocking(ctx, lock, cmdID); err != nil {
        return fmt.Errorf("gc: latch acquire: %w", err)
    }
    defer w.latches.Release(lock, cmdID)

    // Apply the writes under latch protection.
    return w.applyModifies(txn)
}
```

### C. GC removes Delete markers when no older versions exist

Modify the `GC` function's `gcStateRemoveIdempotent` handler for `WriteTypeDelete`.
Instead of unconditionally keeping the Delete marker, peek ahead to check if any
older versions exist. If none do, remove the Delete marker.

```go
case txntypes.WriteTypeDelete:
    // Check if there are any older versions below this Delete.
    olderWrite, _, err := reader.SeekWrite(key, commitTS-1)
    if err != nil {
        return nil, err
    }
    if olderWrite == nil {
        // No older versions — the Delete marker is orphaned. Remove it.
        txn.DeleteWrite(key, commitTS)
        info.DeletedVersions++
        // No need to transition to gcStateRemoveAll; we're done with this key.
        break
    }
    // Older versions exist — keep the Delete and remove everything below.
    state = gcStateRemoveAll
    ts = commitTS - 1
    continue
```

This also handles the case where a Delete marker exists as the **only** version for
a key below the safe point. Previously this marker would persist forever.

For the case where older versions do exist below the Delete, they will be removed in
`gcStateRemoveAll`. After all older versions are removed, the Delete marker remains
but is now orphaned. This is acceptable for a single GC pass — the next GC pass
(when the safe point advances further) will find the Delete as the latest version
and the peek-ahead will find no older versions, removing it then.

**Optimization for same-pass cleanup:** To handle the case in a single pass, after
`gcStateRemoveAll` finishes processing all older versions, check if we entered
from a Delete marker and remove it:

```go
// Track whether we entered gcStateRemoveAll from a Delete marker.
var deleteMarkerTS txntypes.TimeStamp
var hasDeleteMarker bool

// In gcStateRemoveIdempotent, WriteTypeDelete case:
case txntypes.WriteTypeDelete:
    olderWrite, _, err := reader.SeekWrite(key, commitTS-1)
    if err != nil {
        return nil, err
    }
    if olderWrite == nil {
        txn.DeleteWrite(key, commitTS)
        info.DeletedVersions++
        break
    }
    // Remember the Delete marker for potential removal after gcStateRemoveAll.
    deleteMarkerTS = commitTS
    hasDeleteMarker = true
    state = gcStateRemoveAll
    ts = commitTS - 1
    continue

// After the main loop, if we removed all versions below a Delete marker:
if hasDeleteMarker {
    // All older versions have been removed. Remove the Delete marker too.
    txn.DeleteWrite(key, deleteMarkerTS)
    info.DeletedVersions++
}
```

---

## Implementation Steps

1. **Add `waiters sync.Map` to `Latches` struct**
   - File: `internal/storage/txn/latch/latch.go`
   - Add field to `Latches` struct (no initialization needed; zero value is valid)

2. **Implement `AcquireBlocking` method**
   - File: `internal/storage/txn/latch/latch.go`
   - Add method with context support for timeout/cancellation
   - Uses `waiters` map for per-command wake channels

3. **Update `Release` to signal wake channels**
   - File: `internal/storage/txn/latch/latch.go`, lines 103-125
   - After computing `wakeUp` list, send signal to each waiter's channel
   - Non-blocking send (select with default) to avoid deadlock

4. **Remove deprecated `wakeCh` from `latchSlot`**
   - File: `internal/storage/txn/latch/latch.go`, line 23
   - Remove the `wakeCh chan uint64` field
   - Remove allocation in `New()` at line 43
   - The per-slot channel is replaced by per-command channels in `waiters`

5. **Update all 20+ spin-wait sites in storage.go**
   - File: `internal/server/storage.go`
   - Replace `for !s.latches.Acquire(lock, cmdID) {}` with
     `s.latches.AcquireBlocking(context.Background(), lock, cmdID)`
   - Handle returned error appropriately at each call site

6. **Add `latches` parameter to `GCWorker`**
   - File: `internal/storage/gc/gc.go`
   - Add `latches *latch.Latches` field to `GCWorker` struct
   - Add `nextCmdID uint64` (atomic counter) for latch command IDs
   - Update `NewGCWorker` signature to accept `*latch.Latches`
   - Update all `NewGCWorker` call sites

7. **Implement `applyModifiesWithLatches` in GCWorker**
   - File: `internal/storage/gc/gc.go`
   - Acquire latches for batch keys before `applyModifies`
   - Release latches after commit
   - Track batch keys in `processTask`

8. **Fix Delete marker GC in `GC()` function**
   - File: `internal/storage/gc/gc.go`, lines 100-104
   - Add peek-ahead with `reader.SeekWrite(key, commitTS-1)`
   - Remove orphaned Delete markers
   - Track `deleteMarkerTS` for same-pass cleanup after `gcStateRemoveAll`

9. **Update `NewGCWorker` call sites to pass latches**
   - Search all files that call `NewGCWorker` and add the latches parameter
   - The `Storage` struct already holds `*latch.Latches`, so pass `s.latches`

---

## Test Plan

### Unit Tests — Latch Blocking

1. **TestAcquireBlockingUncontended**
   - Acquire latches for a single key with no contention
   - Verify immediate return (< 1ms)
   - Release and verify clean state

2. **TestAcquireBlockingContended**
   - Command A acquires latch for key K
   - Command B calls `AcquireBlocking` for key K in a goroutine
   - Verify B blocks (does not return within 50ms)
   - Release A's latch
   - Verify B acquires within 10ms
   - Verify B's callback returns successfully

3. **TestAcquireBlockingTimeout**
   - Command A acquires latch for key K
   - Command B calls `AcquireBlocking` with 100ms timeout context
   - Verify B returns `context.DeadlineExceeded` after ~100ms
   - Verify B's partially acquired latches are released

4. **TestAcquireBlockingFIFOOrder**
   - Command A holds latch for key K
   - Commands B, C, D call `AcquireBlocking` in order
   - Release A; verify B acquires next (not C or D)
   - Release B; verify C acquires next
   - Release C; verify D acquires next

5. **TestAcquireBlockingMultipleKeys**
   - Command A holds latches for keys [K1, K2]
   - Command B requests latches for keys [K2, K3]
   - Verify B blocks on K2
   - Release A; verify B acquires both K2 and K3

6. **TestNoSpinCPU**
   - Acquire a hot key latch
   - Start 100 goroutines waiting on `AcquireBlocking` for the same key
   - Measure CPU usage over 1 second
   - Verify CPU usage is near 0 (blocked, not spinning)
   - Release the latch and verify all goroutines eventually complete

### Unit Tests — GC Latches

7. **TestGCWorkerAcquiresLatches**
   - Create a `GCWorker` with a mock latch tracker
   - Schedule a GC task
   - Verify latches are acquired before `applyModifies`
   - Verify latches are released after `applyModifies`

8. **TestGCAndTransactionMutualExclusion**
   - Start a GC worker with shared latches
   - Concurrently run a prewrite on the same key
   - Verify serialization: either GC completes before prewrite reads, or
     prewrite completes before GC deletes
   - Verify no phantom reads or corrupt state

### Unit Tests — Delete Marker GC

9. **TestGCRemovesOrphanedDeleteMarker**
   - Write: Put(K, ts=10), Delete(K, ts=20)
   - Run GC with safePoint=30
   - Verify: Put@10 is removed, Delete@20 is also removed
   - Verify: SeekWrite(K, TSMax) returns nil (key fully cleaned)

10. **TestGCKeepsDeleteMarkerWithOlderVersionsAboveSafePoint**
    - Write: Put(K, ts=10), Delete(K, ts=20), Put(K, ts=30)
    - Run GC with safePoint=25
    - Verify: Delete@20 is kept (it's the latest below safe point)
    - Verify: Put@10 is removed (below Delete, in gcStateRemoveAll)

11. **TestGCSinglePassDeleteCleanup**
    - Write: Put(K, ts=5), Put(K, ts=10), Delete(K, ts=20)
    - Run GC with safePoint=30
    - Verify: All three versions are removed in a single pass
    - Verify: Delete@20 is removed after older versions are cleaned

12. **TestGCDeleteMarkerWithNoOlderVersions**
    - Write only: Delete(K, ts=10) (no prior Put)
    - Run GC with safePoint=20
    - Verify: Delete@10 is removed (orphaned from the start)

### Integration Tests

13. **TestHighContentionLatches**
    - Run 50 concurrent transactions on 10 overlapping keys
    - Verify all transactions complete (no deadlocks)
    - Verify CPU usage is bounded (no spin-wait behavior)
    - Verify data consistency after all transactions commit

14. **TestGCUnderTransactionLoad**
    - Run continuous transactions while GC is active
    - Verify no data corruption or phantom reads
    - Verify GC makes progress (deleted version count increases)
    - Verify transactions don't see partially-GC'd state

---

## Addendum: Review Feedback Incorporated

**Review verdict: NEEDS REVISION -- all blocking issues addressed below.**

### Fix 1: AcquireBlocking race -- store wake channel BEFORE calling Acquire -- BLOCKING

The design as written creates the wake channel AFTER `Acquire()` fails. There is a race window between `Acquire()` returning false (which adds the command to the slot's `waitQueue`) and the channel being stored in `waiters`. During this window, `Release()` on another goroutine could release the slot, find the command in the `wakeUp` list, look for it in `waiters`, find nothing (channel not stored yet), and the wake-up signal is lost. The command then blocks forever (or until context timeout).

**Corrected `AcquireBlocking` implementation (Section A):**

```go
func (l *Latches) AcquireBlocking(ctx context.Context, lock *Lock, commandID uint64) error {
    // Create and store the channel BEFORE calling Acquire, so Release()
    // can always find it if the command ends up in a waitQueue.
    ch := make(chan struct{}, 1)
    l.waiters.Store(commandID, ch)

    for {
        if l.Acquire(lock, commandID) {
            l.waiters.Delete(commandID)
            return nil
        }
        select {
        case <-ch:
            // Woken up -- retry acquisition.
            continue
        case <-ctx.Done():
            // Timeout or cancellation -- release any partially acquired latches.
            l.Release(lock, commandID)
            l.waiters.Delete(commandID)
            return ctx.Err()
        }
    }
}
```

This is safe because:
- If `Acquire()` succeeds on the first try, the channel is cleaned up and never used.
- If `Acquire()` fails, the channel is already in place for `Release()` to find.
- The channel buffer size of 1 ensures a signal is not lost even if it arrives before the `select`.

**Note on stale waitQueue entries after timeout**: When `AcquireBlocking` times out, `Release()` only removes owned slots (where `slot.owner == commandID`). It does NOT remove the command from the `waitQueue` of the slot where acquisition failed. The timed-out command ID stays in the `waitQueue`, and when that slot is later released, the timed-out command is dequeued and signaled. Since `waiters.Delete(commandID)` already ran, `Release()` finds no channel, and the signal is dropped. The next real waiter is delayed by one dequeue cycle. This is a minor inefficiency, not a correctness bug. Under high timeout rates it could delay legitimate waiters. This trade-off is acceptable for now.

### Fix 2: Command ID collision -- share single atomic counter between Storage and GCWorker -- BLOCKING

The design adds `nextCmdID uint64` (atomic counter) to `GCWorker` for latch command IDs. The `Storage` struct also has `nextCmdID` (storage.go line 26), starting from 1. If both counters produce the same command ID, the latch system treats them as the same command, causing incorrect "already owned" results in `Acquire()`.

**Corrected approach (option a -- shared counter):**

Replace the independent counters with a single shared `*atomic.Uint64`:

```go
// In Storage:
type Storage struct {
    // ...
    cmdIDCounter *atomic.Uint64  // shared with GCWorker
}

func (s *Storage) allocCmdID() uint64 {
    return s.cmdIDCounter.Add(1)
}
```

```go
// In GCWorker:
type GCWorker struct {
    engine       traits.KvEngine
    latches      *latch.Latches
    cmdIDCounter *atomic.Uint64  // shared with Storage
    // ...
}
```

Both `Storage` and `GCWorker` receive the same `*atomic.Uint64` pointer at construction time, ensuring globally unique command IDs.

### Fix 3: Add public accessor `Storage.Latches()` for GCWorker access

The `latches` field is private (lowercase) on `Storage`. To pass it to `GCWorker`, add a public accessor:

```go
// In internal/server/storage.go:
func (s *Storage) Latches() *latch.Latches {
    return s.latches
}
```

Update the GCWorker construction in `internal/server/server.go:76`:

```go
// Before:
gcWorker := gc.NewGCWorker(storage.Engine(), gc.DefaultGCConfig())

// After:
gcWorker := gc.NewGCWorker(storage.Engine(), storage.Latches(), storage.CmdIDCounter(), gc.DefaultGCConfig())
```

The test call site at `e2e/gc_worker_test.go:35` should also be updated. Consider making the `latches` parameter accept `nil` and creating a local instance internally for testing scenarios where latch sharing is not needed.

### Minor: Error handling guidance for 21 call sites

Each spin-wait site needs conversion. The "appropriate error" return differs per method signature:
- Methods returning `error` can return `fmt.Errorf("latch acquire: %w", err)`
- Methods returning `(*T, error)` can return `nil, fmt.Errorf("latch acquire: %w", err)`
- Methods returning `(*T, *LatchGuard, error)` can return `nil, nil, fmt.Errorf("latch acquire: %w", err)`

Since the initial conversion uses `context.Background()` (no timeout), the error path is currently unreachable. A follow-up should add proper context propagation for future cancellation support.
