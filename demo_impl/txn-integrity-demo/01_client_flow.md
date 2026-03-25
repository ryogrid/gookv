# Client-Side Transaction Flow

## Overview

The transaction integrity demo (`scripts/txn-integrity-demo-verify/main.go`) runs three phases against a 3-node gookv cluster with 1 PD server:

```mermaid
flowchart LR
    P1[Phase 1: Initialize] --> P2[Phase 2: Transfers]
    P2 --> Wait[5s Settle]
    Wait --> P3[Phase 3: Verify]
```

## Constants

| Constant | Value | Purpose |
|----------|-------|---------|
| `numAccounts` | 1000 | Bank accounts to create |
| `initialBalance` | 100 | Starting balance per account ($100) |
| `expectedTotal` | 100,000 | Invariant: sum of all balances |
| `numWorkers` | 32 | Concurrent transfer goroutines |
| `duration` | 30s | Phase 2 runtime |
| `transferMax` | 50 | Max transfer amount |
| `initBatchSize` | 50 | Accounts per seeding transaction |
| `maxTxnRetries` | 50 | Retries per transfer attempt |

Key format: `acct:0000` through `acct:0999` (4-digit zero-padded).
Value format: decimal string (e.g., `"100"`, `"73"`).

---

## Phase 1: Initialize 1000 Accounts

```mermaid
sequenceDiagram
    participant Demo
    participant PD
    participant TxnKV as TxnKVClient
    participant Store as gookv-server

    Demo->>PD: connectPD() + waitForLeader()
    PD-->>Demo: cluster ready

    loop 20 batches (50 accounts each)
        Demo->>TxnKV: Begin(ctx)
        TxnKV->>PD: GetTS()
        PD-->>TxnKV: startTS
        loop 50 accounts
            Demo->>TxnKV: Set(acctKey(i), "100")
            Note over TxnKV: Buffer in membuf (no RPC)
        end
        Demo->>TxnKV: Commit(ctx)
        Note over TxnKV: 2PC: Prewrite all → getCommitTS → commit
        TxnKV->>Store: KvPrewrite(50 mutations)
        Store-->>TxnKV: ok
        TxnKV->>PD: GetTS() → commitTS
        TxnKV->>Store: KvCommit(primary)
        Store-->>TxnKV: ok
        TxnKV->>Store: KvCommit(secondaries, parallel)
    end

    Demo->>TxnKV: Begin(ctx) [read-only]
    loop 1000 accounts
        Demo->>TxnKV: Get(acctKey(i))
        TxnKV->>Store: KvGet(key, startTS)
        Store-->>TxnKV: value
    end
    Demo->>TxnKV: Rollback() [read-only, no commit needed]
    Note over Demo: Assert sum == $100,000

    loop Poll every 2s (60s timeout)
        Demo->>PD: GetRegion(scanKey) [scan all regions]
        PD-->>Demo: region count
    end
    Note over Demo: Wait for >= 3 regions

    loop Poll every 2s (30s timeout)
        Demo->>PD: GetRegion() [check leaders]
        PD-->>Demo: all regions have leaders
    end
```

---

## Phase 2: Concurrent Random Transfers

```mermaid
sequenceDiagram
    participant W as Worker goroutine
    participant TxnKV as TxnKVClient
    participant PD
    participant Store as gookv-server

    Note over W: Pick random from, to (distinct)
    Note over W: Canonical order: first=min, second=max

    W->>TxnKV: Begin(ctx)
    TxnKV->>PD: GetTS() → startTS

    W->>TxnKV: Get(acctKey(from))
    TxnKV->>Store: KvGet(key, startTS)
    Store-->>TxnKV: fromVal

    W->>TxnKV: Get(acctKey(to))
    TxnKV->>Store: KvGet(key, startTS)
    Store-->>TxnKV: toVal

    Note over W: amount = rand(1, min(fromBal, 50))
    Note over W: If fromBal == 0: Rollback + skip

    W->>TxnKV: Set(acctKey(first), newBal)
    Note over TxnKV: Buffer in membuf
    W->>TxnKV: Set(acctKey(second), newBal)
    Note over TxnKV: Buffer in membuf

    W->>TxnKV: Commit(ctx)
    Note over TxnKV: 2PC protocol (see 02_server_flow.md)

    alt Commit succeeds
        Note over W: transfers++, totalMoved += amount
    else ErrWriteConflict / retryable
        W->>TxnKV: Rollback(ctx)
        Note over W: conflicts++, retry with new txn
    else Non-retryable error
        W->>TxnKV: Rollback(ctx)
        Note over W: errCount++, abandon transfer
    end
```

### Transfer Retry Logic

```mermaid
flowchart TD
    A[Pick random from, to] --> B[Begin transaction]
    B --> C[Get from balance]
    C --> D[Get to balance]
    D --> E{fromBal >= 1?}
    E -->|No| F[Rollback, skip]
    E -->|Yes| G[Compute amount]
    G --> H["Set(first), Set(second)"]
    H --> I[Commit]
    I -->|Success| J[transfers++]
    I -->|Retryable error| K[Rollback, conflicts++]
    K --> L{retry < 50?}
    L -->|Yes| B
    L -->|No| M[Give up]
    I -->|Non-retryable| N[Rollback, errCount++]

    style F fill:#ffd,stroke:#aa0
    style J fill:#dfd,stroke:#0a0
    style N fill:#fdd,stroke:#a00
```

### Retryable Errors

| Type | Source | Retried? |
|------|--------|----------|
| `ErrWriteConflict` | Prewrite found newer committed write | Yes |
| `ErrDeadlock` | Pessimistic lock deadlock | Yes |
| `ErrTxnLockNotFound` | Lock disappeared during commit | Yes |
| "key locked" | Prewrite found another txn's lock | Yes |
| "max retries" | SendToRegion exhausted retries | Yes |
| "region error" | Not leader, epoch mismatch | Yes |
| Other errors | gRPC failures, internal errors | No |

---

## Phase 3: Verify Conservation

```mermaid
flowchart TD
    A[Cleanup orphan locks] --> B{Locks found?}
    B -->|Yes| C[KvCleanup on all 3 nodes]
    C --> D[Wait 2s for Raft apply]
    D --> B
    B -->|No or 5 passes| E[Read all balances with SI]
    E --> F{SI read ok?}
    F -->|Yes| G[Compute total]
    F -->|No: lock errors| H[Fallback: RC isolation]
    H --> G
    G --> I{total == $100,000?}
    I -->|Yes| J[PASS]
    I -->|No| K[FAIL]

    style J fill:#dfd,stroke:#0a0
    style K fill:#fdd,stroke:#a00
```

### Cleanup Flow (per pass)

```mermaid
sequenceDiagram
    participant Demo
    participant Node1 as KVS Node 1
    participant Node2 as KVS Node 2
    participant Node3 as KVS Node 3

    Demo->>Node1: KvScanLock(maxTS=MAX, limit=10000)
    Node1-->>Demo: locks[]
    loop Each lock
        Demo->>Node1: KvCleanup(key, lockVersion)
        Note over Node1: Check primary status → commit/rollback
        Node1-->>Demo: ok / error
    end

    Demo->>Node2: KvScanLock(maxTS=MAX, limit=10000)
    Node2-->>Demo: locks[]
    loop Each lock
        Demo->>Node2: KvCleanup(key, lockVersion)
        Node2-->>Demo: ok / error
    end

    Demo->>Node3: KvScanLock(maxTS=MAX, limit=10000)
    Node3-->>Demo: locks[]
    loop Each lock
        Demo->>Node3: KvCleanup(key, lockVersion)
        Node3-->>Demo: ok / error
    end
```

### RC Fallback Read

When SI reads fail due to unresolvable locks, the demo falls back to RC (Read Committed) isolation which skips lock checks entirely:

```mermaid
sequenceDiagram
    participant Demo
    participant KVS as gookv-server (direct gRPC)

    loop 1000 accounts
        Demo->>KVS: KvGet(key, readTS=2^62, IsolationLevel=RC)
        Note over KVS: PointGetter skips lock check (RC mode)
        KVS-->>Demo: committed value (pre-lock)
    end

    Demo->>KVS: KvScanLock(maxTS)
    KVS-->>Demo: remaining locks[]

    loop Each remaining lock
        Demo->>KVS: KvCheckTxnStatus(primary)
        KVS-->>Demo: committed / rolled back
        Note over Demo: If committed: RC value may be stale
    end
```

**Problem with RC fallback**: RC reads the last *committed* write record, which does not include values from prewrite locks whose transactions were committed (primary committed but secondary lock not yet resolved). This causes balance discrepancies.
