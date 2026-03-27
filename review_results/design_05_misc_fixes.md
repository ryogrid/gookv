# Design Doc 05: Miscellaneous Fixes

## Issues Covered

| ID | Severity | File | Description |
|----|----------|------|-------------|
| Client M4 | Major | `pkg/client/lock_resolver.go:22-23,69` | `resolving` map grows unboundedly |
| Raft M4 | Major | `internal/raftstore/split/checker.go:140-191` | `scanRegionSize` picks split key from first CF only |
| Raft M5 | Major | `internal/raftstore/merge.go:118-140` | `ExecCommitMerge` dead code and fragile fallback |
| Server P1 | Performance | `internal/server/flow/flow.go:112-125` | `ReadPool.Stop()` doesn't drain pending tasks |
| m5 | Maintenance | Multiple files | Replace deprecated `grpc.Dial` with `grpc.NewClient` |

---

## Issue 1: Client M4 -- LockResolver resolving Map Grows Unboundedly

### Problem

In `pkg/client/lock_resolver.go`, the `resolving` map deduplicates concurrent lock resolution attempts:

```go
// pkg/client/lock_resolver.go:16-23
type LockResolver struct {
    sender   *RegionRequestSender
    cache    *RegionCache
    pdClient pdclient.Client

    mu        sync.Mutex
    resolving map[lockKey]chan struct{}
}
```

Entries are inserted at line 69 and deleted in a `defer` at lines 72-77:

```go
// pkg/client/lock_resolver.go:68-77
ch := make(chan struct{})
lr.resolving[lk] = ch
lr.mu.Unlock()

defer func() {
    lr.mu.Lock()
    delete(lr.resolving, lk)
    lr.mu.Unlock()
    close(ch)
}()
```

While individual entries are deleted after resolution, Go maps never shrink their backing array. After resolving thousands of distinct locks, the map's internal hash table retains memory for all buckets ever allocated, even though the map is logically near-empty. In long-running workloads with high contention, this constitutes an unbounded memory leak.

### Design

Periodically recreate the `resolving` map to allow GC of the old backing array. The simplest approach is to replace the map with a fresh one when it becomes empty after a resolve. Since the map is typically very small (most of the time 0-5 entries), we can check `len(lr.resolving)` after each delete and recreate when it reaches 0.

An alternative is to recreate on a size threshold (e.g., when `len(resolving) == 0` but the map has been used for > N resolutions). The simple approach is sufficient here because the map is guarded by a mutex and access patterns are bursty.

### Implementation Steps

1. **Add a resolution counter** to `LockResolver` (`lock_resolver.go:16-23`):

```go
type LockResolver struct {
    sender   *RegionRequestSender
    cache    *RegionCache
    pdClient pdclient.Client

    mu             sync.Mutex
    resolving      map[lockKey]chan struct{}
    resolveCount   int // tracks total resolutions since last map reset
}
```

2. **Update the defer block** in `resolveSingleLock()` (`lock_resolver.go:72-77`):

```go
defer func() {
    lr.mu.Lock()
    delete(lr.resolving, lk)
    lr.resolveCount++
    // Periodically recreate the map to release old backing memory.
    // Threshold of 1000 avoids churning on every single resolve.
    if len(lr.resolving) == 0 && lr.resolveCount >= 1000 {
        lr.resolving = make(map[lockKey]chan struct{})
        lr.resolveCount = 0
    }
    lr.mu.Unlock()
    close(ch)
}()
```

3. **No change to `NewLockResolver()`** -- `resolveCount` zero-value is correct.

### Test Plan

1. **Unit test**: Resolve 2000 distinct locks sequentially. After all resolutions complete, verify `resolving` map is empty. (Memory behavior is hard to test directly, but we verify the counter reset logic.)
2. **Unit test**: Resolve 5 locks concurrently on the same `lockKey`. Verify only one actual RPC is made (existing dedup test).
3. **Existing tests**: Run client-side tests to verify no regression in lock resolution.

---

## Issue 2: Raft M4 -- scanRegionSize Picks Split Key from First CF Only

### Problem

In `internal/raftstore/split/checker.go:140-191`, `scanRegionSize()` iterates CFs sequentially (`CFDefault`, `CFWrite`, `CFLock`):

```go
// internal/raftstore/split/checker.go:146-188
for _, cf := range []string{cfnames.CFDefault, cfnames.CFWrite, cfnames.CFLock} {
    // ...
    for iter.SeekToFirst(); iter.Valid(); iter.Next() {
        // ...
        totalSize += entrySize

        if splitKey == nil && totalSize >= halfSize {
            rawKey, _, decErr := mvcc.DecodeKey(key)
            if decErr == nil && rawKey != nil {
                splitKey = mvcc.EncodeLockKey(rawKey)
            } else {
                splitKey = append([]byte(nil), key...)
            }
        }

        if totalSize >= w.cfg.MaxSize {
            iter.Close()
            return totalSize, splitKey, nil
        }
    }
    // ...
}
```

The split key is selected when `totalSize >= halfSize`. Since CFs are iterated sequentially, the split key is always chosen from whichever CF's cumulative contribution first pushes `totalSize` past `halfSize`. If `CFDefault` is large, the split key comes from `CFDefault` keys only. If `CFDefault` is small but `CFWrite` is large, the split key comes from `CFWrite`.

**The problem**: The "midpoint" is computed across the combined size of all CFs scanned so far, but the key is sampled from whichever CF happens to be scanning when the threshold is crossed. This may not represent the actual data distribution. For example, if `CFDefault` has 30MB and `CFWrite` has 66MB (total 96MB, halfSize = 48MB), the split key is picked from `CFWrite` at the 18MB mark within `CFWrite` (48-30=18). But the data-heavy CF is `CFWrite`, and a split key at 18MB into `CFWrite` might split `CFWrite` into 18/48, not 48/48.

In TiKV, the split checker considers each CF's contribution and picks the split key from the dominant CF to ensure the split is roughly balanced across the CF that contributes most data.

### Design

Two-pass approach:
1. **First pass**: Scan each CF to compute its total size. Track the dominant CF (highest total size).
2. **Second pass**: Scan only the dominant CF to find the key at approximately `totalSize / 2` within that CF's data.

However, this doubles the I/O. A more efficient single-pass approach:

**Single-pass with per-CF tracking**: Track `perCFSize[cf]` and `perCFSplitKey[cf]` during the existing sequential scan. For each CF, record the key at the CF-local midpoint (`perCFSize[cf] / 2`). After scanning all CFs, pick the split key from the CF with the largest total size.

Since we don't know each CF's total size until after scanning it, we record the key at `perCFSize[cf] >= totalSize/2` as a candidate. But `totalSize` is not known until all CFs are scanned.

**Revised approach**: Since this is approximate anyway, use a simpler fix:

Track the split key candidate **per CF** (key at that CF's local midpoint), then after scanning all CFs, select the candidate from the CF with the largest total size. Each CF's midpoint is `cfSize / 2` of that CF.

### Implementation Steps

1. **Refactor `scanRegionSize()`** (`checker.go:140-191`):

```go
func (w *SplitCheckWorker) scanRegionSize(startKey, endKey []byte) (uint64, []byte, error) {
    var totalSize uint64
    cfs := []string{cfnames.CFDefault, cfnames.CFWrite, cfnames.CFLock}

    // Per-CF tracking.
    type cfInfo struct {
        size     uint64
        splitKey []byte // candidate key at local midpoint
    }
    cfData := make(map[string]*cfInfo, len(cfs))
    for _, cf := range cfs {
        cfData[cf] = &cfInfo{}
    }

    for _, cf := range cfs {
        info := cfData[cf]
        opts := traits.IterOptions{
            LowerBound: startKey,
        }
        if len(endKey) > 0 {
            opts.UpperBound = endKey
        }

        iter := w.engine.NewIterator(cf, opts)

        for iter.SeekToFirst(); iter.Valid(); iter.Next() {
            key := iter.Key()
            value := iter.Value()
            entrySize := uint64(len(key) + len(value))
            totalSize += entrySize
            info.size += entrySize

            // Record candidate split key at this CF's local midpoint.
            // We don't know final cfSize yet, so we use a running check:
            // record the first key where cfSize >= totalSoFar/2 won't work.
            // Instead, just record every key and pick later -- too expensive.
            // Compromise: record the key at cfSize >= (w.cfg.SplitSize / 2)
            // contribution from this CF. This approximates the midpoint
            // assuming this CF dominates.
            if info.splitKey == nil && info.size >= w.cfg.SplitSize/2 {
                rawKey, _, decErr := mvcc.DecodeKey(key)
                if decErr == nil && rawKey != nil {
                    info.splitKey = mvcc.EncodeLockKey(rawKey)
                } else {
                    info.splitKey = append([]byte(nil), key...)
                }
            }

            if totalSize >= w.cfg.MaxSize {
                iter.Close()
                goto selectKey
            }
        }

        if err := iter.Error(); err != nil {
            iter.Close()
            return 0, nil, fmt.Errorf("split: scan %s: %w", cf, err)
        }
        iter.Close()
    }

selectKey:
    // Pick split key from the CF with the largest total size.
    var splitKey []byte
    var maxCFSize uint64
    for _, cf := range cfs {
        info := cfData[cf]
        if info.size > maxCFSize && info.splitKey != nil {
            maxCFSize = info.size
            splitKey = info.splitKey
        }
    }

    // Fallback: if no CF-specific key was found (all CFs tiny),
    // use the original global midpoint approach.
    if splitKey == nil {
        halfSize := totalSize / 2
        var runningSize uint64
        for _, cf := range cfs {
            opts := traits.IterOptions{LowerBound: startKey}
            if len(endKey) > 0 {
                opts.UpperBound = endKey
            }
            iter := w.engine.NewIterator(cf, opts)
            for iter.SeekToFirst(); iter.Valid(); iter.Next() {
                runningSize += uint64(len(iter.Key()) + len(iter.Value()))
                if runningSize >= halfSize {
                    rawKey, _, decErr := mvcc.DecodeKey(iter.Key())
                    if decErr == nil && rawKey != nil {
                        splitKey = mvcc.EncodeLockKey(rawKey)
                    } else {
                        splitKey = append([]byte(nil), iter.Key()...)
                    }
                    iter.Close()
                    return totalSize, splitKey, nil
                }
            }
            iter.Close()
        }
    }

    return totalSize, splitKey, nil
}
```

**Simplified alternative** (recommended): Rather than the two-pass fallback, use a simpler threshold. Record the per-CF candidate when `cfSize >= cfSize_at_end / 2`, but since we don't know `cfSize_at_end` during scanning, use a fraction of `SplitSize` as the threshold. This is already approximate, so using `SplitSize/2` as the per-CF threshold is reasonable. The dominant CF is likely to exceed this threshold, while minor CFs won't, so only the dominant CF's candidate will be non-nil.

### Test Plan

1. **Unit test**: Create a region with 80MB in `CFWrite` and 16MB in `CFDefault`. Run split check. Verify the split key originates from `CFWrite` data (not `CFDefault`).
2. **Unit test**: Create a region with equal data in all CFs. Verify a split key is selected (any CF is acceptable).
3. **Unit test**: Create a region below `SplitSize`. Verify no split key is returned.
4. **Existing tests**: Run split-related e2e tests in `e2e_external/region_split_test.go`.

---

## Issue 3: Raft M5 -- ExecCommitMerge Dead Code and Fragile Fallback

### Problem

In `internal/raftstore/merge.go:102-155`, `ExecCommitMerge()` has three branches for merge direction:

```go
// internal/raftstore/merge.go:118-140
if len(source.GetStartKey()) > 0 && bytes.Equal(target.GetEndKey(), source.GetStartKey()) {
    // Right merge: source is right-adjacent to target.
    merged.EndKey = append([]byte(nil), source.GetEndKey()...)
} else if len(source.GetEndKey()) > 0 && bytes.Equal(target.GetStartKey(), source.GetEndKey()) {
    // Left merge: source is left-adjacent to target.
    merged.StartKey = append([]byte(nil), source.GetStartKey()...)
} else {
    // Fallback: extend to cover both ranges.
    if len(source.GetStartKey()) > 0 && (len(merged.GetStartKey()) == 0 || bytes.Compare(source.GetStartKey(), merged.GetStartKey()) < 0) {
        merged.StartKey = append([]byte(nil), source.GetStartKey()...)
    }
    if len(source.GetEndKey()) > 0 && (len(merged.GetEndKey()) == 0 || bytes.Compare(source.GetEndKey(), merged.GetEndKey()) > 0) {
        merged.EndKey = append([]byte(nil), source.GetEndKey()...)
    }
    if len(source.GetEndKey()) == 0 {
        merged.EndKey = nil
    }
}
```

Additionally, lines 118-121 contain dead code:

```go
if bytes.Equal(target.GetEndKey(), source.GetStartKey()) ||
    (len(target.GetEndKey()) == 0 && len(source.GetStartKey()) == 0) {
    // This shouldn't happen for right merge -- let's handle both cases.
}
```

This is an empty if-block that does nothing -- likely leftover from development.

**The fallback branch (lines 130-140) is dangerous**: If two non-adjacent regions somehow reach `ExecCommitMerge`, the fallback silently extends the merged region to cover both ranges, potentially overlapping with intermediate regions. This creates a key-range overlap that corrupts the region map. Non-adjacent merges should be rejected as an error -- they indicate a bug in the scheduling layer.

In TiKV, `exec_commit_merge` only handles adjacent regions and returns an error otherwise.

### Design

1. Remove the dead code block (lines 118-121).
2. Replace the fallback branch with an error return for non-adjacent regions.
3. Keep the right-merge and left-merge branches as-is.

### Implementation Steps

1. **Update `ExecCommitMerge()`** (`merge.go:102-155`):

```go
func ExecCommitMerge(target *metapb.Region, source *metapb.Region) (*CommitMergeResult, error) {
    if target == nil || source == nil {
        return nil, fmt.Errorf("merge: nil target or source")
    }

    merged := cloneMergeRegion(target)
    if merged.RegionEpoch == nil {
        merged.RegionEpoch = &metapb.RegionEpoch{}
    }

    // Determine merge direction. Source must be adjacent to target.
    if len(source.GetStartKey()) > 0 && bytes.Equal(target.GetEndKey(), source.GetStartKey()) {
        // Right merge: source is right-adjacent to target.
        merged.EndKey = append([]byte(nil), source.GetEndKey()...)
    } else if len(source.GetEndKey()) > 0 && bytes.Equal(target.GetStartKey(), source.GetEndKey()) {
        // Left merge: source is left-adjacent to target.
        merged.StartKey = append([]byte(nil), source.GetStartKey()...)
    } else {
        return nil, fmt.Errorf("merge: source region %d [%x, %x) is not adjacent to target region %d [%x, %x)",
            source.GetId(), source.GetStartKey(), source.GetEndKey(),
            target.GetId(), target.GetStartKey(), target.GetEndKey())
    }

    // Set epoch: max(source.version, target.version) + 1.
    srcVer := source.GetRegionEpoch().GetVersion()
    tgtVer := merged.RegionEpoch.Version
    if srcVer > tgtVer {
        merged.RegionEpoch.Version = srcVer + 1
    } else {
        merged.RegionEpoch.Version = tgtVer + 1
    }

    return &CommitMergeResult{
        Region: merged,
        Source: source,
    }, nil
}
```

2. **Remove the dead code block** that was at lines 118-121 (already addressed by the rewrite above).

### Test Plan

1. **Unit test: right merge** -- Target `[a, m)`, source `[m, z)`. Verify merged is `[a, z)`.
2. **Unit test: left merge** -- Target `[m, z)`, source `[a, m)`. Verify merged is `[a, z)`.
3. **Unit test: non-adjacent regions** -- Target `[a, f)`, source `[m, z)`. Verify error returned.
4. **Unit test: unbounded regions** -- Target `[a, "")` (end of keyspace), source with `EndKey = ""`. Verify correct handling.
5. **Existing tests**: Run merge-related tests if any exist.

---

## Issue 4: Server P1 -- ReadPool.Stop() Doesn't Drain Pending Tasks

### Problem

In `internal/server/flow/flow.go:112-125`, `Stop()` simply closes `stopCh`, and worker goroutines exit immediately:

```go
// internal/server/flow/flow.go:112-114
func (rp *ReadPool) Stop() {
    close(rp.stopCh)
}

// internal/server/flow/flow.go:116-125
func (rp *ReadPool) worker() {
    for {
        select {
        case task := <-rp.taskCh:
            task()
        case <-rp.stopCh:
            return
        }
    }
}
```

When `stopCh` is closed, Go's `select` randomly chooses between the two ready cases. If `taskCh` has pending tasks, some may be executed and some may not -- it's non-deterministic. Tasks that are never executed include submitted work that callers may be waiting on (e.g., via channels embedded in the task closure). This causes goroutine leaks and hung callers during shutdown.

### Design

After `stopCh` is signaled, each worker should drain remaining tasks from `taskCh` before exiting. The worker loop should first detect the stop signal, then consume all remaining tasks in `taskCh`.

### Implementation Steps

1. **Update `worker()`** (`flow.go:116-125`):

```go
func (rp *ReadPool) worker() {
    for {
        select {
        case task := <-rp.taskCh:
            task()
        case <-rp.stopCh:
            // Drain remaining tasks before exiting.
            for {
                select {
                case task := <-rp.taskCh:
                    task()
                default:
                    return
                }
            }
        }
    }
}
```

2. **Add `sync.WaitGroup` to wait for all workers to finish** (`flow.go:37-44`):

```go
type ReadPool struct {
    workers    int
    taskCh     chan func()
    ewmaSlice  atomic.Int64
    queueDepth atomic.Int64
    alpha      float64
    stopCh     chan struct{}
    wg         sync.WaitGroup
}
```

3. **Update `NewReadPool()`** to track workers (`flow.go:47-63`):

```go
func NewReadPool(workers int) *ReadPool {
    if workers <= 0 {
        workers = 4
    }
    rp := &ReadPool{
        workers: workers,
        taskCh:  make(chan func(), workers*16),
        alpha:   0.3,
        stopCh:  make(chan struct{}),
    }

    rp.wg.Add(workers)
    for i := 0; i < workers; i++ {
        go func() {
            defer rp.wg.Done()
            rp.worker()
        }()
    }

    return rp
}
```

4. **Update `Stop()` to wait for drain** (`flow.go:112-114`):

```go
func (rp *ReadPool) Stop() {
    close(rp.stopCh)
    rp.wg.Wait()
}
```

5. **Add `"sync"` import** to `flow.go` if not already present.

### Test Plan

1. **Unit test: drain on stop** -- Submit 100 tasks that each increment an atomic counter. Immediately call `Stop()`. After `Stop()` returns, verify the counter equals 100 (all tasks executed).
2. **Unit test: stop blocks until drain** -- Submit a task that sleeps 100ms. Call `Stop()` and verify it blocks for at least ~100ms (the task runs to completion).
3. **Existing tests**: Run flow/flow tests to verify no regression.

---

## Issue 5: m5 -- Replace Deprecated grpc.Dial with grpc.NewClient

### Problem

`grpc.Dial` and `grpc.DialContext` are deprecated in favor of `grpc.NewClient` since gRPC-Go v1.63. The deprecated APIs have different default behaviors (e.g., `grpc.Dial` creates a connection lazily by default but `grpc.NewClient` always creates lazily, and the error semantics differ).

### Current Usages (gookv source, excluding `tikv/`)

| File | Line | Current Code |
|------|------|-------------|
| `e2e/helpers_test.go` | 15 | `grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))` |
| `internal/pd/server_test.go` | 26 | `grpc.Dial(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))` |
| `pkg/e2elib/helpers.go` | 267 | `grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))` |
| `internal/server/server_test.go` | 35 | `grpc.Dial(srv.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))` |
| `internal/server/server_test.go` | 681 | `grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))` |
| `internal/pd/forward.go` | 43 | `grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))` |

### Design

Replace all `grpc.Dial(...)` calls with `grpc.NewClient(...)`. Key differences:
- `grpc.NewClient` requires the target as the first argument (same as `grpc.Dial`).
- `grpc.NewClient` does NOT support `grpc.WithBlock()` (it's always non-blocking).
- Error is returned only for invalid configuration, not for connection failure.
- The target format should be `passthrough:///addr` or just `addr` (passthrough is default).

Since all current usages use `grpc.WithTransportCredentials(insecure.NewCredentials())` and don't use `grpc.WithBlock()`, the migration is straightforward.

### Implementation Steps

For each file, replace `grpc.Dial(` with `grpc.NewClient(`:

1. **`e2e/helpers_test.go:15`**:
```go
// Before:
conn, err := grpc.Dial(addr,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)

// After:
conn, err := grpc.NewClient(addr,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

2. **`internal/pd/server_test.go:26`**:
```go
// Before:
conn, err := grpc.Dial(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))

// After:
conn, err := grpc.NewClient(s.Addr(), grpc.WithTransportCredentials(insecure.NewCredentials()))
```

3. **`pkg/e2elib/helpers.go:267`**:
```go
// Before:
conn, err := grpc.Dial(addr,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)

// After:
conn, err := grpc.NewClient(addr,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

4. **`internal/server/server_test.go:35`**:
```go
// Before:
conn, err := grpc.Dial(
    srv.Addr(),
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)

// After:
conn, err := grpc.NewClient(
    srv.Addr(),
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

5. **`internal/server/server_test.go:681`**:
```go
// Before:
conn, err := grpc.Dial(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))

// After:
conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(insecure.NewCredentials()))
```

6. **`internal/pd/forward.go:43`**:
```go
// Before:
conn, err := grpc.Dial(addr,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)

// After:
conn, err := grpc.NewClient(addr,
    grpc.WithTransportCredentials(insecure.NewCredentials()),
)
```

### Test Plan

1. **Build verification**: `make build` succeeds without deprecation warnings.
2. **Unit tests**: Run all tests in affected packages (`e2e`, `internal/pd`, `internal/server`, `pkg/e2elib`). All should pass -- `grpc.NewClient` is a drop-in replacement for non-blocking `grpc.Dial`.
3. **E2E tests**: Run `e2e_external` tests to verify real connections work.
4. **Static analysis**: Run `staticcheck` or `go vet` to confirm no remaining deprecation warnings for gRPC dial.

---

## Files Modified

| File | Lines | Change |
|------|-------|--------|
| `pkg/client/lock_resolver.go` | 16-23, 72-77 | Add `resolveCount`; recreate map when empty after threshold |
| `internal/raftstore/split/checker.go` | 140-191 | Per-CF split key tracking; pick key from dominant CF |
| `internal/raftstore/merge.go` | 102-155 | Remove dead code; replace fallback with error return |
| `internal/server/flow/flow.go` | 37-44, 47-63, 112-125 | Add WaitGroup; drain taskCh on stop; Stop() waits |
| `e2e/helpers_test.go` | 15 | `grpc.Dial` -> `grpc.NewClient` |
| `internal/pd/server_test.go` | 26 | `grpc.Dial` -> `grpc.NewClient` |
| `pkg/e2elib/helpers.go` | 267 | `grpc.Dial` -> `grpc.NewClient` |
| `internal/server/server_test.go` | 35, 681 | `grpc.Dial` -> `grpc.NewClient` |
| `internal/pd/forward.go` | 43 | `grpc.Dial` -> `grpc.NewClient` |

## Risk Assessment

- **Client M4 (map GC)**: Very low risk. The map recreation only happens when the map is empty, so no concurrent readers are affected.
- **Raft M4 (split key)**: Medium risk. Changes the heuristic for split key selection, which affects region balance. The new approach should produce better-balanced splits but may change behavior for existing workloads.
- **Raft M5 (merge fallback)**: Low risk. Converts a silent corruption into an explicit error. If non-adjacent merges are somehow triggered in production, this will surface the bug rather than hide it.
- **Server P1 (drain)**: Low risk. Workers drain tasks before exiting, which is strictly better than dropping them. The `WaitGroup` ensures `Stop()` blocks until all tasks complete.
- **m5 (grpc.NewClient)**: Low risk. Drop-in replacement. The only behavioral difference is that `grpc.NewClient` never dials eagerly, but all current usages already use lazy (non-blocking) semantics.

---

## Addendum: Review Feedback Incorporated

**Review verdicts:**
- **Client M4 (LockResolver Map GC): PASS** -- no changes needed.
- **Raft M4 (Split Key from Dominant CF): NEEDS REVISION** -- blocking issue addressed below.
- **Raft M5 (ExecCommitMerge Dead Code): PASS** -- no changes needed.
- **Server P1 (ReadPool Drain): PASS** -- no changes needed.
- **m5 (grpc.Dial Migration): NEEDS REVISION** -- blocking issue addressed below.

### Fix for Raft M4: Single-pass approach tracking per-CF midpoints, select from dominant CF

The per-CF threshold `SplitSize/2` is wrong for non-dominant CFs. If no single CF individually exceeds `SplitSize/2`, no candidates are recorded at all, and the code falls through to the expensive two-pass fallback. This happens when data is evenly spread across CFs (e.g., 30MB default + 30MB write + 30MB lock = 90MB total, but no single CF exceeds 48MB). The two-pass fallback doubles I/O, which is expensive for regions near `MaxSize`.

**Corrected approach -- single-pass with running midpoint tracking:**

Instead of using `SplitSize/2` as the per-CF threshold, track per-CF running midpoints during the single-pass scan. For each CF, continuously update the candidate split key to the key seen when `cfRunningSize` is closest to `cfRunningSize / 2` (i.e., approximately the midpoint of that CF's data seen so far). After scanning all CFs, select the candidate from the CF with the largest `cfSize`.

Concretely, record the split key for each CF at the point where `cfSize` first crosses half of that CF's *running* total. Since the running total only grows, we record the candidate when `cfSize` crosses the current `cfSize / 2` threshold. A simple approach: record the key the first time the CF's running size doubles past any previously recorded midpoint. In practice, the simplest correct approach is:

```go
type cfInfo struct {
    size           uint64
    splitKey       []byte
    splitKeyAtSize uint64  // cfSize when splitKey was recorded
}

// During iteration for each CF:
info.size += entrySize

// Update midpoint candidate: always keep the key closest to cfSize/2.
// Record when cfSize first exceeds 2 * splitKeyAtSize (i.e., the midpoint
// has shifted enough to warrant an update).
if info.splitKey == nil || info.size >= 2*info.splitKeyAtSize {
    rawKey, _, decErr := mvcc.DecodeKey(key)
    if decErr == nil && rawKey != nil {
        info.splitKey = mvcc.EncodeLockKey(rawKey)
    } else {
        info.splitKey = append([]byte(nil), key...)
    }
    info.splitKeyAtSize = info.size
}
```

After scanning all CFs, pick the candidate from the dominant CF:

```go
// selectKey:
var splitKey []byte
var maxCFSize uint64
for _, cf := range cfs {
    info := cfData[cf]
    if info.size > maxCFSize && info.splitKey != nil {
        maxCFSize = info.size
        splitKey = info.splitKey
    }
}

// Fallback: if no candidate was found (all CFs empty), use nil.
// No second scan needed.
```

This avoids the two-pass fallback entirely. The midpoint candidate from the dominant CF approximates the true midpoint of that CF's data, which produces better-balanced splits. The key recorded is at approximately `cfSize/2` of the dominant CF, which is the desired split point.

**Remove the two-pass fallback** from the implementation. The single-pass approach with running midpoints handles all cases, including evenly distributed data across CFs.

### Fix for m5: Handle grpc.WithBlock() sites and list ALL DialContext usages

The design doc incorrectly claims "all current usages ... don't use `grpc.WithBlock()`." Multiple call sites use `grpc.WithBlock()`, and there are significantly more `grpc.DialContext` usages than listed.

**Missing from the design doc (non-tikv gookv source):**

| File | Line | API | Uses WithBlock? |
|------|------|-----|-----------------|
| `internal/pd/transport.go` | 121 | `grpc.DialContext` | No |
| `internal/server/transport/transport.go` | 284 | `grpc.DialContext` | No |
| `pkg/client/request_sender.go` | 153 | `grpc.DialContext` | No |
| `pkg/pdclient/client.go` | 157, 211, 240 | `grpc.DialContext` | **Yes** |
| `scripts/txn-demo-verify/main.go` | 523 | `grpc.DialContext` | **Yes** |
| `scripts/pd-failover-demo-verify/main.go` | 488 | `grpc.DialContext` | **Yes** |
| `scripts/scale-demo-verify/main.go` | 391 | `grpc.DialContext` | **Yes** |
| `scripts/txn-integrity-demo-verify/main.go` | 765, 869, 932 | `grpc.DialContext` | **Yes** |
| `scripts/pd-cluster-verify/main.go` | 28, 93, 169 | `grpc.DialContext` | **Yes** |

**Critical issue**: `grpc.NewClient` does NOT support `grpc.WithBlock()` -- it is always non-blocking. Migrating `WithBlock()` sites requires changing the connection establishment pattern.

**Corrected migration strategy:**

1. **For the 6 `grpc.Dial` sites listed in the doc** (which do NOT use `grpc.WithBlock()`): Proceed as designed -- straightforward `grpc.Dial` to `grpc.NewClient` replacement.

2. **For `grpc.DialContext` sites WITHOUT `grpc.WithBlock()`** (`internal/pd/transport.go`, `internal/server/transport/transport.go`, `pkg/client/request_sender.go`): Also migrate to `grpc.NewClient`. Note that `grpc.DialContext` with a context timeout constrains connection establishment, but `grpc.NewClient` ignores context -- these sites should be migrated carefully, potentially adding `conn.Connect()` if the timeout behavior was intentional.

3. **For sites using `grpc.WithBlock()`** (`pkg/pdclient/client.go`, all scripts): Either **defer migration** or implement an alternative blocking mechanism:
   - Use `grpc.NewClient` without `WithBlock()`
   - After creating the client, call `conn.Connect()` followed by polling `conn.WaitForStateChange()` to ensure connectivity before proceeding
   - For `pdclient` specifically, `grpc.WithBlock()` is intentional to ensure the connection is established before `GetMembers()` is called. Simply removing `WithBlock()` would change semantics, though gRPC RPCs do wait for connectivity by default with `WaitForReady`

4. **Scripts in `scripts/`** should be included in the migration scope if comprehensive coverage is intended.

**Recommended phased approach:**
- Phase 1: Migrate the 6 `grpc.Dial` sites (already in the doc) and the 3 `grpc.DialContext` sites without `WithBlock()`.
- Phase 2: Migrate `pkg/pdclient/client.go` with `WithBlock()` replacement pattern.
- Phase 3: Migrate verification scripts.
