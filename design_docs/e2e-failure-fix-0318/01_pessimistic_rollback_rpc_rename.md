# Fix: KVPessimisticRollback gRPC Method Name Mismatch

## Problem

The server defines the handler method as `KvPessimisticRollback` (lowercase `v`), but the proto-generated `TikvServer` interface expects `KVPessimisticRollback` (uppercase `V`). As a result, the server method does not satisfy the interface — the embedded `UnimplementedTikvServer` stub is called instead, returning `Unimplemented` to all unary gRPC callers.

The `BatchCommands` code path calls `svc.KvPessimisticRollback(...)` directly (line 771), so batch routing works but uses the wrong name and would break if the method were renamed without updating the call site.

See: `e2e-failure/0318/pessimistic_rollback_rpc_mismatch.md`

## Root Cause

A typo in the method receiver name at `internal/server/server.go:445`. The method was named `KvPessimisticRollback` instead of `KVPessimisticRollback`.

## Changes

### File: `internal/server/server.go`

#### 1. Rename handler method (line 445)

```go
// Before
func (svc *tikvService) KvPessimisticRollback(ctx context.Context, req *kvrpcpb.PessimisticRollbackRequest) (*kvrpcpb.PessimisticRollbackResponse, error)

// After
func (svc *tikvService) KVPessimisticRollback(ctx context.Context, req *kvrpcpb.PessimisticRollbackRequest) (*kvrpcpb.PessimisticRollbackResponse, error)
```

#### 2. Update BatchCommands call site (line 771)

```go
// Before
r, _ := svc.KvPessimisticRollback(ctx, cmd.PessimisticRollback)

// After
r, _ := svc.KVPessimisticRollback(ctx, cmd.PessimisticRollback)
```

## Verification

1. `go build ./internal/server/...` — confirms the method now satisfies the `TikvServer` interface.
2. `go test ./e2e/...` — `TestTxnPessimisticRollbackRPCMismatch` should pass (no longer returns `Unimplemented`).