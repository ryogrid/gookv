# TxnClient API Design

## 1. TxnKVClient

```go
// TxnKVClient provides transactional key-value operations using Percolator 2PC.
type TxnKVClient struct {
    sender   *RegionRequestSender
    cache    *RegionCache
    pdClient pdclient.Client
    resolver *LockResolver
}
```

Methods:
- `NewTxnKVClient(sender *RegionRequestSender, cache *RegionCache, pdClient pdclient.Client) *TxnKVClient`
- `Begin(ctx context.Context, opts ...TxnOption) (*TxnHandle, error)` — gets start_ts from PD, creates TxnHandle
- `Close() error`

## 2. TxnOption and TxnOptions

```go
type TxnOption func(*TxnOptions)

type TxnOptions struct {
    Mode           TxnMode  // Optimistic (default) or Pessimistic
    UseAsyncCommit bool     // Enable async commit optimization
    Try1PC         bool     // Try 1PC if all mutations in single region
    LockTTL        uint64   // Lock TTL in milliseconds (default: 3000)
}

type TxnMode int
const (
    TxnModeOptimistic  TxnMode = iota
    TxnModePessimistic
)
```

Option constructors: `WithPessimistic()`, `WithAsyncCommit()`, `With1PC()`, `WithLockTTL(ms uint64)`

## 3. TxnHandle

```go
// TxnHandle represents an in-progress transaction.
type TxnHandle struct {
    mu       sync.Mutex
    startTS  txntypes.TimeStamp
    opts     TxnOptions

    // Buffered mutations
    membuf   map[string]mutationEntry // key -> mutation

    // Pessimistic state
    lockKeys map[string]struct{} // keys with acquired pessimistic locks

    // State
    committed bool
    rolledBack bool

    // Dependencies
    client *TxnKVClient
}

type mutationEntry struct {
    op    kvrpcpb.Op // Put or Del
    value []byte     // nil for Del
}
```

Methods:
- `Get(ctx context.Context, key []byte) ([]byte, error)` — reads from membuf first, then KvGet RPC. On lock encounter, calls LockResolver then retries.
- `BatchGet(ctx context.Context, keys [][]byte) ([]KvPair, error)` — same pattern with KvBatchGet RPC, parallel by region
- `Set(key, value []byte)` — buffers in membuf. In pessimistic mode, also acquires pessimistic lock immediately via KvPessimisticLock.
- `Delete(key []byte)` — buffers deletion in membuf. In pessimistic mode, acquires pessimistic lock.
- `Commit(ctx context.Context) error` — creates twoPhaseCommitter and runs 2PC
- `Rollback(ctx context.Context) error` — if prewrite started, sends KvBatchRollback; if pessimistic locks held, sends PessimisticRollback
- `StartTS() txntypes.TimeStamp` — returns start_ts

## 4. twoPhaseCommitter (internal)

```go
type twoPhaseCommitter struct {
    client    *TxnKVClient
    startTS   txntypes.TimeStamp
    commitTS  txntypes.TimeStamp
    mutations []mutationWithKey
    primary   []byte // primary key (first after sorting)
    opts      TxnOptions

    prewriteDone bool
}

type mutationWithKey struct {
    key   []byte
    op    kvrpcpb.Op
    value []byte
}
```

Methods:
- `execute(ctx context.Context) error` — orchestrates: selectPrimary -> prewrite -> getCommitTS -> commitPrimary -> commitSecondaries
- `prewrite(ctx context.Context) error` — groups mutations by region, prewrites primary region first, then secondaries in parallel
- `commitPrimary(ctx context.Context) error` — commits primary key synchronously
- `commitSecondaries(ctx context.Context)` — commits secondary keys in parallel (best-effort)
- `rollback(ctx context.Context) error` — rolls back all prewritten keys via KvBatchRollback

## 5. LockResolver

```go
type LockResolver struct {
    sender   *RegionRequestSender
    cache    *RegionCache
    pdClient pdclient.Client

    // Dedup concurrent resolutions for same lock
    mu       sync.Mutex
    resolved map[lockKey]struct{}
}

type lockKey struct {
    primary string // hex of primary key
    startTS uint64
}
```

Methods:
- `ResolveLocks(ctx context.Context, locks []*kvrpcpb.LockInfo) (resolvedCount int, err error)` — iterates locks, calls checkTxnStatus on primary, then resolveLock
- `checkTxnStatus(ctx context.Context, primary []byte, lockTS txntypes.TimeStamp) (*kvrpcpb.CheckTxnStatusResponse, error)` — sends KvCheckTxnStatus to primary key's region
- `resolveLock(ctx context.Context, lock *kvrpcpb.LockInfo, commitTS txntypes.TimeStamp) error` — sends KvResolveLock to the encountered key's region with commitTS (0 = rollback, >0 = commit)

## 6. Client Extension

```go
// Add to existing Client struct (client.go)
func (c *Client) TxnKV() *TxnKVClient {
    return NewTxnKVClient(c.sender, c.cache, c.pdClient)
}
```

## 7. Error Types

```go
var (
    ErrTxnCommitted  = errors.New("txn: already committed")
    ErrTxnRolledBack = errors.New("txn: already rolled back")
    ErrWriteConflict = errors.New("txn: write conflict")
    ErrTxnLockNotFound = errors.New("txn: lock not found")
    ErrDeadlock      = errors.New("txn: deadlock")
)
```
