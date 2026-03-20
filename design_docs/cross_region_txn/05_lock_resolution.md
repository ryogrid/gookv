# Lock Resolution

## 1. When Locks Are Encountered
During Get/BatchGet/Scan RPCs, the server may return `KeyError.Locked` containing `LockInfo`. This happens when the read encounters another transaction's lock in CF_LOCK at a version <= the read's snapshot (start_ts).

Required LockInfo fields (see 06_server_side_changes.md for the fix):
- `Key`: the key that was locked
- `PrimaryLock`: primary key of the locking transaction (from Lock.Primary)
- `LockVersion`: start_ts of the locking transaction (from Lock.StartTS)
- `LockTtl`: TTL in milliseconds (from Lock.TTL)
- `LockType`: type of lock (Put, Del, Lock, PessimisticLock)
- `UseAsyncCommit`: whether the locking txn uses async commit
- `MinCommitTs`: for async commit resolution
- `Secondaries`: for async commit resolution (on primary key's lock)

## 2. Percolator Lock Resolution Protocol

### 2.1 Resolution Sequence Diagram

```mermaid
sequenceDiagram
    participant C as Client
    participant RX as Region-X (encountered lock)
    participant RP as Region-P (primary key's region)

    C->>RX: KvGet(key, read_ts)
    RX-->>C: KeyError{Locked: LockInfo{Key, PrimaryLock, LockVersion, LockTtl}}
    Note over C: Extract primary_key = LockInfo.PrimaryLock<br/>lock_ts = LockInfo.LockVersion
    C->>RP: KvCheckTxnStatus(primary_key, lock_ts, caller_start_ts, current_ts, rollback_if_not_exist=true)
    Note over RP: Check primary key's state
    alt Lock exists and TTL not expired
        RP-->>C: (lock_ttl, 0, Action_NoAction)
        Note over C: Lock alive - backoff and retry original read
    else Lock exists and TTL expired
        Note over RP: Rollback primary
        RP-->>C: (0, 0, Action_TTLExpireRollback)
        C->>RX: KvResolveLock(start_version=lock_ts, commit_version=0)
        Note over RX: Rollback the lock
        C->>RX: Retry KvGet(key, read_ts)
    else No lock, commit record exists
        RP-->>C: (0, commit_ts, Action_NoAction)
        C->>RX: KvResolveLock(start_version=lock_ts, commit_version=commit_ts)
        Note over RX: Commit the lock
        C->>RX: Retry KvGet(key, read_ts)
    else No lock, rollback record exists
        RP-->>C: (0, 0, Action_NoAction)
        C->>RX: KvResolveLock(start_version=lock_ts, commit_version=0)
        Note over RX: Rollback the lock
        C->>RX: Retry KvGet(key, read_ts)
    else No lock, no records
        Note over RP: Write rollback record
        RP-->>C: (0, 0, Action_LockNotExistRollback)
        C->>RX: KvResolveLock(start_version=lock_ts, commit_version=0)
        Note over RX: Rollback the lock
        C->>RX: Retry KvGet(key, read_ts)
    end
```

### 2.2 Why This Works
The primary key's commit record in CF_WRITE is the single source of truth for transaction fate. Every secondary lock stores Lock.Primary pointing to the primary key. By checking the primary's status, any client can determine whether to commit or rollback a secondary lock. This is the core insight of the Percolator protocol.

## 3. Async Commit Lock Resolution
For locks with UseAsyncCommit=true:
1. Read the primary lock's Secondaries list
2. Send KvCheckSecondaryLocks to check all secondary keys
3. If all secondary locks exist: compute commit_ts = max(min_commit_ts across all keys)
4. If any secondary is missing (already resolved): the primary's status has been determined -- check CF_WRITE
5. Resolve all locks with the computed commit_ts

## 4. Integration Flowchart

```mermaid
flowchart TD
    A[TxnHandle.Get called] --> B{Key in membuf?}
    B -->|Yes| C[Return buffered value]
    B -->|No| D[Send KvGet RPC]
    D --> E{Response type?}
    E -->|Success| F[Return value]
    E -->|KeyError.Locked| G[Extract LockInfo]
    G --> H[Call LockResolver.ResolveLocks]
    H --> I[checkTxnStatus on primary]
    I --> J{Primary status?}
    J -->|Committed| K[resolveLock with commit_ts]
    J -->|Rolled back| L[resolveLock with commit_ts=0]
    J -->|Lock alive| M[Backoff wait]
    K --> N[Retry KvGet]
    L --> N
    M --> N
    N --> O{Max retries exceeded?}
    O -->|No| D
    O -->|Yes| P[Return error]
```

## 5. LockResolver Deduplication
Use sync.Mutex + map to avoid concurrent resolutions for the same (primary, start_ts):
```go
type lockKey struct {
    primary string
    startTS uint64
}
// Before resolving, check if already in progress
// After resolving, record in resolved map
```

## 6. Backoff Strategy for Lock Waits
When lock is alive (TTL not expired):
- Initial backoff: 20ms
- Max backoff: 2000ms (2s)
- Multiplier: 2x
- Jitter: +/-25%
- Max retries: configurable (default: 20, covering ~40s of lock waits)
