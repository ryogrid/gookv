# Server-Side Transaction Processing

## Overview

Each client RPC goes through three layers on the server:

```mermaid
flowchart TD
    RPC[gRPC Handler<br/>server.go] --> Storage[Storage Layer<br/>storage.go]
    Storage --> TxnActions[Transaction Actions<br/>actions.go]
    TxnActions --> MVCC[MVCC Layer<br/>mvcc/txn.go, reader.go]
    MVCC --> Engine[Pebble Engine]

    Storage -->|cluster mode| Raft[Raft Consensus]
    Raft --> Engine
```

## LatchGuard Pattern

All write operations in cluster mode use the LatchGuard pattern to prevent the latch-Raft race condition:

```mermaid
sequenceDiagram
    participant Handler as RPC Handler
    participant Storage as Storage.*Modifies()
    participant Latch as Latches
    participant Raft as Raft (ProposeModifies)
    participant Engine as Pebble Engine

    Handler->>Storage: *Modifies(keys, ...)
    Storage->>Latch: Acquire(keys)
    Note over Latch: Spin until acquired
    Storage->>Engine: NewSnapshot()
    Engine-->>Storage: snapshot
    Note over Storage: Compute MVCC modifications
    Storage-->>Handler: (modifies, guard)

    Note over Handler: Latch still held via guard

    Handler->>Raft: ProposeModifies(regionID, modifies)
    Note over Raft: Raft consensus + apply
    Raft->>Engine: Write modifications
    Raft-->>Handler: ok

    Handler->>Latch: ReleaseLatch(guard)
    Note over Latch: Next waiter can proceed
```

**Key invariant**: The latch is held from snapshot creation through Raft apply completion. This ensures a concurrent transaction on the same keys will see the applied modifications in its snapshot.

---

## KvPrewrite (2PC Phase 1)

```mermaid
sequenceDiagram
    participant Client
    participant Handler as KvPrewrite Handler
    participant Storage
    participant Actions as txn.Prewrite()
    participant Raft

    Client->>Handler: KvPrewrite(mutations, primary, startTS)
    Handler->>Storage: PrewriteModifies(mutations, primary, startTS, lockTTL)
    Note over Storage: Acquire latch on all mutation keys

    loop Each mutation
        Storage->>Actions: Prewrite(txn, reader, props, mutation)

        Actions->>Actions: LoadLock(key)
        alt Lock exists (different txn)
            Actions-->>Storage: KeyLockedError{key, lock}
        else No lock
            Actions->>Actions: SeekWrite(key, TSMax)
            alt Write exists with commitTS > startTS
                Actions-->>Storage: ErrWriteConflict
            else No conflict
                Actions->>Actions: PutLock(key, lock)
                Note over Actions: Write short_value in lock if small
                Actions-->>Storage: nil (success)
            end
        end
    end

    Storage-->>Handler: (modifies, errors, guard)
    Note over Handler: defer ReleaseLatch(guard)

    alt No errors
        Handler->>Raft: ProposeModifies(regionID, modifies)
        Raft-->>Handler: ok
    end

    Handler-->>Client: PrewriteResponse
```

### Prewrite Conflict Detection

```mermaid
flowchart TD
    A[Load lock from CF_LOCK] --> B{Lock exists?}
    B -->|Yes, same startTS| C[Idempotent: return nil]
    B -->|Yes, different startTS| D["Return KeyLockedError<br/>(carries lock details for resolution)"]
    B -->|No| E[SeekWrite from CF_WRITE]
    E --> F{Write exists with<br/>commitTS > startTS?}
    F -->|Yes, Put or Delete| G[Return ErrWriteConflict]
    F -->|Yes, Rollback/Lock| H[Skip: not a data conflict]
    F -->|No| I[Write prewrite lock to CF_LOCK]
    H --> I
    I --> J[Write value to CF_DEFAULT if large]
    J --> K[Return nil: success]

    style D fill:#fdd,stroke:#a00
    style G fill:#fdd,stroke:#a00
    style K fill:#dfd,stroke:#0a0
```

---

## KvCommit (2PC Phase 2)

```mermaid
sequenceDiagram
    participant Client
    participant Handler as KvCommit Handler
    participant Storage
    participant Actions as txn.Commit()
    participant Raft

    Client->>Handler: KvCommit(keys, startTS, commitTS)
    Handler->>Storage: CommitModifies(keys, startTS, commitTS)
    Note over Storage: Acquire latch

    loop Each key
        Storage->>Actions: Commit(txn, reader, key, startTS, commitTS)
        Actions->>Actions: LoadLock(key)
        alt Lock missing or startTS mismatch
            Actions-->>Storage: ErrTxnLockNotFound
        else Lock found, matches
            Actions->>Actions: UnlockKey (delete CF_LOCK)
            Actions->>Actions: PutWrite (write to CF_WRITE with commitTS)
            Note over Actions: Write record includes short_value from lock
            Actions-->>Storage: nil
        end
    end

    Storage-->>Handler: (modifies, error, guard)
    Handler->>Raft: ProposeModifies(regionID, modifies)
    Raft-->>Handler: ok
    Handler-->>Client: CommitResponse
```

---

## KvGet (Transactional Read)

```mermaid
sequenceDiagram
    participant Client
    participant Handler as KvGet Handler
    participant Storage
    participant PG as PointGetter

    Client->>Handler: KvGet(key, version, isolationLevel)
    Handler->>Storage: GetWithIsolation(key, version, level)
    Storage->>Storage: NewSnapshot()
    Storage->>PG: NewPointGetter(reader, version, level)
    Storage->>PG: Get(key)

    alt SI Isolation
        PG->>PG: LoadLock(key)
        alt Lock exists with startTS <= readTS
            PG-->>Storage: LockError{key, lock}
            Storage-->>Handler: error
            Handler-->>Client: KeyError{Locked: lockInfo}
        else No blocking lock
            PG->>PG: GetWrite(key, readTS) → find visible write
            PG->>PG: Read value (short_value or CF_DEFAULT)
            PG-->>Storage: value
        end
    else RC Isolation
        Note over PG: Skip lock check entirely
        PG->>PG: GetWrite(key, readTS) → find visible write
        PG->>PG: Read value
        PG-->>Storage: value
    end

    Storage-->>Handler: value
    Handler-->>Client: GetResponse{value}
```

---

## KvBatchRollback

```mermaid
sequenceDiagram
    participant Client
    participant Handler as KvBatchRollback Handler
    participant Storage
    participant Actions as txn.Rollback()
    participant Raft

    Client->>Handler: KvBatchRollback(keys, startTS)
    Handler->>Storage: BatchRollbackModifies(keys, startTS)
    Note over Storage: Acquire latch on all keys

    loop Each key
        Storage->>Actions: Rollback(txn, reader, key, startTS)
        Actions->>Actions: GetTxnCommitRecord(key, startTS)
        alt Already committed
            Actions-->>Storage: ErrAlreadyCommitted
        else Already rolled back
            Actions-->>Storage: nil (idempotent)
        else Not found
            Actions->>Actions: LoadLock(key)
            alt Lock exists, matches startTS
                Actions->>Actions: UnlockKey (delete CF_LOCK)
                Actions->>Actions: Delete CF_DEFAULT value if put prewrite
            end
            Actions->>Actions: PutWrite(rollback record, CF_WRITE)
            Actions-->>Storage: nil
        end
    end

    Storage-->>Handler: (modifies, error, guard)
    Handler->>Raft: ProposeModifies(regionID, modifies)
    Raft-->>Handler: ok
    Handler-->>Client: BatchRollbackResponse
```

---

## KvCleanup

Single-key lock cleanup that checks the **primary key's status** to determine commit vs rollback:

```mermaid
flowchart TD
    A[Load key's commit/rollback record] --> B{Record exists?}
    B -->|Committed| C["Return commitTS (no modify needed)"]
    B -->|Rolled back| D["Return 0 (no modify needed)"]
    B -->|No record| E[Load lock from CF_LOCK]
    E --> F{Lock exists with matching startTS?}
    F -->|No| G["Return 0 (no lock to clean)"]
    F -->|Yes| H[Read lock.Primary]
    H --> I[CheckTxnStatus on primary key]
    I --> J{Primary status?}
    J -->|Committed| K["ResolveLock: commit this key<br/>(UnlockKey + PutWrite with commitTS)"]
    J -->|Rolled back / not found| L["Rollback this key<br/>(UnlockKey + PutWrite rollback record)"]
    J -->|Error| L

    K --> M[ProposeModifies via Raft]
    L --> M

    style C fill:#dfd
    style D fill:#dfd
    style G fill:#ffd
    style K fill:#ddf
    style L fill:#fdd
```

---

## KvCheckTxnStatus (with Cleanup)

```mermaid
flowchart TD
    A[Load lock for primaryKey] --> B{Lock exists<br/>with matching startTS?}
    B -->|Yes| C{"TTL expired?<br/>(physical time comparison)"}
    C -->|"Yes: lockTime + TTL <= callerTime"| D["Force rollback:<br/>UnlockKey + delete value + write rollback"]
    C -->|No: lock still alive| E["Return IsLocked=true"]
    B -->|No| F{Commit/rollback record?}
    F -->|Committed| G["Return CommitTS"]
    F -->|Rolled back| H["Return IsRolledBack=true"]
    F -->|Not found| I{rollbackIfNotExist?}
    I -->|Yes| J["Write protective rollback record"]
    J --> K["Return IsRolledBack=true"]
    I -->|No| L["Return empty status"]

    D --> M[ProposeModifies via Raft]

    style D fill:#fdd,stroke:#a00
    style E fill:#ffd,stroke:#aa0
    style G fill:#dfd,stroke:#0a0
    style J fill:#ddf,stroke:#00a
```

---

## Region Routing

### resolveRegionID

When `req.GetContext().GetRegionId()` is 0 (no region context from client), the server resolves the region by encoding the raw user key:

```go
func resolveRegionID(key []byte) uint64 {
    encodedKey := mvcc.EncodeLockKey(key)  // codec.EncodeBytes(nil, key)
    return coord.ResolveRegionForKey(encodedKey)
}
```

### ResolveRegionForKey

Iterates all local peers, compares key against each region's `[startKey, endKey)` boundaries, and selects the narrowest match (largest startKey).

### groupModifiesByRegion

Used by `proposeModifiesToRegionsWithRegionError` for multi-region operations:

```mermaid
flowchart LR
    M[Modify with MVCC-encoded key] --> D[DecodeKey → rawKey]
    D --> E["EncodeLockKey(rawKey)<br/>(strip timestamp)"]
    E --> R[resolveRegionID]
    R --> G[Group into region bucket]
```

This ensures CF_LOCK keys (no timestamp) and CF_WRITE keys (with timestamp) for the same user key route to the same region.
