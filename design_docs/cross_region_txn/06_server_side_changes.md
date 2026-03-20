# Server-Side Changes Required

## 1. Overview
Two categories of server-side changes are required before TxnClient can work in cluster mode.

## 2. Category A: Multi-Region Raft Proposal Routing

### 2.1 Problem
In cluster mode, transactional RPC handlers use `coord.ProposeModifies(regionID, modifies, timeout)` which routes all modifications to a single region. For cross-region transactions, modifications must be routed to their correct regions.

### 2.2 Existing Solution
`proposeModifiesToRegions()` already exists at `internal/server/server.go:L807-816`. It groups modifications by region via `groupModifiesByRegion()` and proposes to each region separately. Also `proposeModifiesToRegionsWithRegionError()` at L818-831 returns region errors.

### 2.3 Required Changes

| RPC Handler | Current Code (server.go) | Line | Current Approach | Required Fix |
|-------------|-------------------------|------|------------------|--------------|
| KvPrewrite (standard 2PC) | `coord.ProposeModifies(regionID, modifies, ...)` | L371 | Single region via resolveRegionID(primary) | Use `proposeModifiesToRegions()` |
| KvPrewrite (async commit) | `coord.ProposeModifies(regionID, modifies, ...)` | L334 | Single region | Use `proposeModifiesToRegions()` |
| KvPrewrite (1PC) | `coord.ProposeModifies(regionID, modifies, ...)` | L289 | Single region | No change needed (1PC is single-region by definition) |
| KvCommit | `coord.ProposeModifies(regionID, modifies, ...)` | L410 | Single region via resolveRegionID(keys[0]) | Use `proposeModifiesToRegions()` |
| KvPessimisticLock | `coord.ProposeModifies(regionID, modifies, ...)` | L549 | Single region via resolveRegionID(primary) | Use `proposeModifiesToRegions()` |
| KvBatchRollback | `coord.ProposeModifies(...)` | ~L470 | Single region | Use `proposeModifiesToRegions()` |
| KvResolveLock | `coord.ProposeModifies(regionID, modifies, ...)` | L613 | Single region via resolveRegionID(reqKeys[0]) | Use `proposeModifiesToRegions()` |

Handlers that need NO change (operate on single key):
- KvCheckTxnStatus: operates on single primary key
- KvTxnHeartBeat: operates on single primary key
- KvCleanup: operates on single key
- KvCheckSecondaryLocks: read-only

### 2.4 Implementation Pattern
For each handler, replace:
```go
// Before (single-region)
regionID := resolveRegionID(someKey)
coord.ProposeModifies(regionID, modifies, timeout)

// After (multi-region)
svc.proposeModifiesToRegions(coord, modifies, timeout)
```

## 3. Category B: Lock Info Propagation (CRITICAL)

### 3.1 Problem
The read path (KvGet, KvBatchGet, KvScan) returns incomplete LockInfo when encountering another transaction's lock. Without full lock details, the client cannot resolve locks via the Percolator protocol.

### 3.2 Current State

**PointGetter.Get()** (`internal/storage/mvcc/point_getter.go:L59-71`):
- Line 62: Loads lock via `reader.LoadLock(key)`
- Line 66: Returns bare `ErrKeyIsLocked` error — no lock details attached to the error
- The actual `*txntypes.Lock` struct is available at this point but discarded

**KvGet handler** (`internal/server/server.go:L181-206`):
- Line 187: Checks `errors.Is(err, mvcc.ErrKeyIsLocked)`
- Lines 188-193: Returns `LockInfo` but can only set `Key` and `Version` from the request
- Missing: `PrimaryLock`, `LockVersion` (start_ts of locking txn), `LockTtl`, `LockType`

**KvBatchGet handler** (`internal/server/server.go:L430-461`):
- Lines 443-450: Similar incomplete LockInfo

**KvScan handler** (`internal/server/server.go:L208-237`):
- Lines 219-224: Similar incomplete LockInfo

### 3.3 Required Fields in LockInfo

| LockInfo Field | Source (txntypes.Lock) | Purpose |
|---------------|----------------------|---------|
| PrimaryLock | lock.Primary | Identifies the primary key for CheckTxnStatus |
| LockVersion | lock.StartTS | start_ts of locking transaction |
| LockTtl | lock.TTL | Determines if lock is expired |
| Key | (the encountered key) | Key that was locked |
| LockType | lock.LockType (mapped to kvrpcpb.Op) | Type of lock operation |
| UseAsyncCommit | lock.UseAsyncCommit | Triggers async commit resolution path |
| MinCommitTs | lock.MinCommitTS | For async commit commit_ts computation |
| Secondaries | lock.Secondaries | For async commit resolution |

### 3.4 Recommended Fix: Option A — Structured Error

Change PointGetter/Scanner to return a structured error containing the Lock:

```go
// New error type in internal/storage/mvcc/
type LockError struct {
    Key  []byte
    Lock *txntypes.Lock
}

func (e *LockError) Error() string {
    return fmt.Sprintf("mvcc: key %x is locked by txn %d", e.Key, e.Lock.StartTS)
}

func (e *LockError) Is(target error) bool {
    return target == ErrKeyIsLocked
}
```

Update PointGetter.Get():
```go
// Before (point_getter.go:L66)
return nil, ErrKeyIsLocked

// After
return nil, &LockError{Key: key, Lock: lock}
```

Update KvGet handler:
```go
// Before (server.go:L187-193)
if errors.Is(err, mvcc.ErrKeyIsLocked) {
    resp.Error = &kvrpcpb.KeyError{
        Locked: &kvrpcpb.LockInfo{Key: req.GetKey()},
    }
}

// After
var lockErr *mvcc.LockError
if errors.As(err, &lockErr) {
    resp.Error = &kvrpcpb.KeyError{
        Locked: lockToLockInfo(lockErr.Key, lockErr.Lock),
    }
}
```

Note: `lockToLockInfo()` already exists at server.go:L676-705 and correctly maps all Lock fields to LockInfo.

### 3.5 Option B — Re-read Lock (Alternative)
Have the handler re-read the lock when ErrKeyIsLocked is received:
```go
if errors.Is(err, mvcc.ErrKeyIsLocked) {
    lock, _ := reader.LoadLock(key)
    if lock != nil {
        resp.Error = &kvrpcpb.KeyError{Locked: lockToLockInfo(key, lock)}
    }
}
```
This is simpler but requires an extra read. Option A is recommended for efficiency.

### 3.6 Scanner Changes
Same pattern as PointGetter — the Scanner in `internal/storage/mvcc/scanner.go` also encounters locks and should return LockError instead of bare ErrKeyIsLocked.
