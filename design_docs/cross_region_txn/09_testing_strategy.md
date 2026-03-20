# Testing Strategy

## 1. Unit Tests

### 1.1 TxnHandle Tests (txn_test.go)
- Membuf: Set/Get/Delete buffer correctly, delete overrides set, last-write-wins
- Primary key selection: deterministic (first key after sort), single key = primary = no secondaries
- Mutation collection: membuf entries correctly converted to kvrpcpb.Mutation slice
- State management: cannot Set after Commit, cannot Commit twice, Rollback after Commit errors

### 1.2 twoPhaseCommitter Tests (committer_test.go)
- Mutation grouping: correct assignment of mutations to regions via GroupKeysByRegion
- Request construction: PrewriteRequest has correct PrimaryLock, StartVersion, LockTtl, TxnSize
- Primary identification: primary's region group sent first
- Async commit fields: UseAsyncCommit, Secondaries set correctly on primary's request
- 1PC detection: TryOnePc only set when all mutations in one region

### 1.3 LockResolver Tests (lock_resolver_test.go)
- checkTxnStatus → committed: resolveLock called with commit_ts > 0
- checkTxnStatus → rolled back: resolveLock called with commit_ts = 0
- checkTxnStatus → lock alive: returns without resolving (caller should backoff)
- Deduplication: concurrent ResolveLocks for same (primary, start_ts) only sends one CheckTxnStatus
- Async commit resolution: CheckSecondaryLocks path

## 2. Integration / E2E Tests

### 2.1 Single-Region 2PC (txnkv_test.go)
- Begin → Set("k1","v1") → Set("k2","v2") → Commit → verify Get returns values
- Begin → Set → Rollback → verify Get returns empty

### 2.2 Cross-Region 2PC
- Setup: 3 regions with known key ranges
- Begin → Set keys spanning 3 regions → Commit → verify all keys readable
- Begin → Set keys spanning 3 regions → Rollback → verify no keys readable

### 2.3 Write Conflict
- Txn1: Begin → Set("k1") → Commit
- Txn2: Begin (before Txn1 commits) → Set("k1") → Commit → expect WriteConflict error

### 2.4 Lock Resolution
- Txn1: Begin → Prewrite "k1" (but don't commit — simulate crash)
- Txn2: Begin → Get("k1") → should trigger lock resolution
- Wait for Txn1's lock TTL to expire
- Txn2's Get should eventually succeed (lock resolved by rollback)

### 2.5 Lock Resolution — Committed Primary
- Txn1: Begin → Set("k1", "v1") in region-A, Set("k2", "v2") in region-B
- Txn1: Prewrite both → Commit primary (k1) → simulate failure before committing k2
- Txn2: Begin → Get("k2") → lock encountered → resolve → CheckTxnStatus on k1 → committed → commit k2's lock
- Txn2: Get("k2") returns "v2"

### 2.6 Pessimistic Locking
- Txn1 (pessimistic): Begin → Set("k1") → acquires pessimistic lock
- Txn2 (pessimistic): Begin → Set("k1") → should block or timeout (ErrKeyIsLocked)
- Txn1: Commit → Txn2 can now proceed

### 2.7 Async Commit
- Begin with UseAsyncCommit → Set keys → Commit → verify txn committed without explicit commit_ts from PD
- Verify min_commit_ts correctly computed

### 2.8 1PC Optimization
- Begin with Try1PC → Set keys all in same region → Commit
- Verify no locks written to CF_LOCK (writes go directly to CF_WRITE)
- Verify commit_ts returned from prewrite response

### 2.9 Concurrent Transactions
- Launch N goroutines, each running Begin → Set(unique key) → Commit
- Verify all N keys are correctly committed with no data loss
- Launch N goroutines writing to overlapping keys, verify at most one succeeds per key

## 3. Test Helpers

```go
// setupTestCluster creates a multi-region test cluster
func setupTestCluster(t *testing.T, regionCount int) (*Client, func())

// mustBegin starts a transaction, fails test on error
func mustBegin(t *testing.T, client *TxnKVClient, opts ...TxnOption) *TxnHandle

// mustCommit commits a transaction, fails test on error
func mustCommit(t *testing.T, txn *TxnHandle)

// assertKeyValue reads a key and asserts its value
func assertKeyValue(t *testing.T, client *TxnKVClient, key, expectedValue []byte)

// assertKeyNotFound reads a key and asserts it doesn't exist
func assertKeyNotFound(t *testing.T, client *TxnKVClient, key []byte)
```

## 4. Test Matrix

| Scenario | Single Region | Multi Region | Pessimistic | Async Commit | 1PC |
|----------|:---:|:---:|:---:|:---:|:---:|
| Basic commit | x | x | x | x | x |
| Rollback | x | x | x | — | — |
| Write conflict | x | x | x | x | x |
| Lock resolution | x | x | — | x | — |
| Concurrent txns | x | x | x | — | — |
| Crash recovery | — | x | — | x | — |
