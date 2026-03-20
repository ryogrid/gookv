# Concurrency Model

## 1. Parallel Prewrite/Commit via errgroup
Reuse the same pattern as RawKVClient.BatchGet (rawkv.go:L151-191):

```go
g, gCtx := errgroup.WithContext(ctx)
var mu sync.Mutex
var results []*kvrpcpb.PrewriteResponse

for regionID, group := range regionGroups {
    regionID, group := regionID, group // capture
    g.Go(func() error {
        resp, err := sendPrewrite(gCtx, group)
        if err != nil {
            return err
        }
        mu.Lock()
        results = append(results, resp)
        mu.Unlock()
        return nil
    })
}
if err := g.Wait(); err != nil {
    return err
}
```

Key principle: Primary region is always sent FIRST (synchronously), secondary regions are sent in parallel AFTER primary succeeds.

## 2. TxnHandle Thread Safety
- `sync.Mutex` protects `membuf`, `lockKeys`, `committed`, `rolledBack` fields
- All public methods (Get, Set, Delete, Commit, Rollback) acquire the lock
- Get releases the lock before making RPCs, re-acquires after
- Membuf reads during Get don't need lock (if we enforce single-goroutine usage for mutations)
- Alternative: document that TxnHandle is NOT goroutine-safe for mutations, only for reads

Recommended approach: Document TxnHandle as NOT safe for concurrent mutations. Multiple concurrent Gets are safe.

## 3. Lock Resolver Deduplication
Prevent multiple goroutines from resolving the same lock simultaneously:

```go
type LockResolver struct {
    // ...
    mu        sync.Mutex
    resolving map[lockKey]chan struct{} // in-flight resolutions
}

func (lr *LockResolver) ResolveLocks(ctx context.Context, locks []*kvrpcpb.LockInfo) error {
    for _, lock := range locks {
        key := lockKey{primary: string(lock.PrimaryLock), startTS: lock.LockVersion}

        lr.mu.Lock()
        if ch, ok := lr.resolving[key]; ok {
            lr.mu.Unlock()
            <-ch // wait for in-flight resolution
            continue
        }
        ch := make(chan struct{})
        lr.resolving[key] = ch
        lr.mu.Unlock()

        // Resolve lock...
        err := lr.doResolve(ctx, lock)
        close(ch) // notify waiters

        lr.mu.Lock()
        delete(lr.resolving, key)
        lr.mu.Unlock()

        if err != nil {
            return err
        }
    }
    return nil
}
```

Alternative: use `golang.org/x/sync/singleflight` for simpler implementation.

## 4. Pessimistic Lock Heartbeat
For pessimistic transactions, locks must be kept alive to prevent expiration:

```go
func (t *TxnHandle) startHeartbeat(ctx context.Context) {
    interval := time.Duration(t.opts.LockTTL/2) * time.Millisecond
    ticker := time.NewTicker(interval)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            // Send KvTxnHeartBeat for primary key
            t.client.sendHeartbeat(ctx, t.primaryKey(), t.startTS, t.opts.LockTTL)
        }
    }
}
```

Heartbeat goroutine:
- Started when first pessimistic lock is acquired
- Sends KvTxnHeartBeat every lockTTL/2 to the primary key's region
- Stopped on Commit or Rollback
- Uses context cancellation for cleanup

## 5. Goroutine Lifecycle

```
TxnKVClient.Begin()
    └── creates TxnHandle
         ├── Get() → may spawn LockResolver goroutines (bounded by lock count)
         ├── Set() (pessimistic) → may start heartbeat goroutine
         └── Commit()
              ├── Prewrite: 1 goroutine per secondary region (errgroup)
              ├── Commit primary: 1 goroutine (synchronous)
              ├── Commit secondaries: 1 goroutine per region (errgroup, best-effort)
              └── Stops heartbeat goroutine
```

## 6. Context Propagation
- All RPCs receive the caller's context
- Commit secondaries use a detached context (background) since they're best-effort even if caller cancels
- Heartbeat uses a dedicated context cancelled on Commit/Rollback
