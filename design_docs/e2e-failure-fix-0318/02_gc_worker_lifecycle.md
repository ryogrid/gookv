# Fix: GCWorker Background Goroutine Never Started

## Problem

`NewServer()` creates a `GCWorker` via `gc.NewGCWorker(...)` but never calls its `Start()` method. The `Start()` method launches the background goroutine that reads tasks from the internal channel and executes them. Without it, the `KvGC` gRPC handler enqueues a task and blocks forever on the callback channel, because no goroutine is consuming from the task queue.

Similarly, `Server.Stop()` does not call `gcWorker.Stop()`, so even after the fix the goroutine would leak on shutdown.

See: `e2e-failure/0318/gc_worker_not_started.md`

## Root Cause

Missing lifecycle management for `GCWorker` in `internal/server/server.go`. The worker was created but never started or stopped.

## Changes

### File: `internal/server/server.go`

#### 1. Start the GCWorker in `NewServer()` (after line 60)

Add `gcWorker.Start()` immediately after construction:

```go
gcWorker := gc.NewGCWorker(storage.Engine(), gc.DefaultGCConfig())
gcWorker.Start() // ADD
```

#### 2. Stop the GCWorker in `Stop()` (lines 116-120)

Add `gcWorker.Stop()` before `GracefulStop()` so in-flight GC tasks can drain:

```go
func (s *Server) Stop() {
    s.cancel()
    if s.gcWorker != nil {
        s.gcWorker.Stop() // ADD
    }
    s.grpcServer.GracefulStop()
    s.wg.Wait()
}
```

## Verification

1. `go build ./internal/server/...` — compiles cleanly.
2. `go test ./e2e/...` — `TestGCWorkerCleansOldVersions` and `TestGCWorkerMultipleKeys` should pass without hanging.
3. Manual check: start a server, call `KvGC` via grpcurl, confirm it returns promptly with a success response.