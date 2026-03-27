# Design Doc 06: Test Improvements

## Issues Covered

| ID | Severity | File | Description |
|----|----------|------|-------------|
| Test 1.1 | Test Gap | `e2e/gc_worker_test.go:42-102` | GC test doesn't verify old version was deleted |
| Test 5.1 | Test Duplication | `e2e_external/cluster_raw_kv_test.go:16-33`, `e2e_external/client_lib_test.go:17-36` | Duplicate `newClusterWithLeader`/`newClientCluster` helpers |

---

## Issue 1: Test 1.1 -- GC Test Doesn't Verify Old Version Was Deleted

### Problem

In `e2e/gc_worker_test.go:42-102`, `TestGCWorkerCleansOldVersions` writes two versions of a key (commit TS 20 and 40), runs GC with safe point 35, and then only verifies the **latest** version is still readable:

```go
// e2e/gc_worker_test.go:95-99
getResp, err := client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-key"), Version: 50})
require.NoError(t, err)
assert.False(t, getResp.GetNotFound())
assert.Equal(t, []byte("new-val"), getResp.GetValue(), "latest version should be preserved")
```

The test **never** verifies that the old version (commit TS 20) was actually garbage collected. A GC implementation that does nothing would pass this test -- the latest version would still be readable regardless.

The same issue exists in `TestGCWorkerMultipleKeys` (lines 104-171): it only checks new versions, never checks old versions are gone.

### Current Code

```go
// e2e/gc_worker_test.go:42-102
func TestGCWorkerCleansOldVersions(t *testing.T) {
    addr, _, gcWorker := startStandaloneServerWithGC(t)
    _, client := dialTikvClient(t, addr)
    ctx := context.Background()

    // Write version 1: startTS=10, commitTS=20
    // Write version 2: startTS=30, commitTS=40
    // GC safe point: 35
    // ...

    // The latest version should still be readable.
    getResp, err := client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-key"), Version: 50})
    require.NoError(t, err)
    assert.False(t, getResp.GetNotFound())
    assert.Equal(t, []byte("new-val"), getResp.GetValue(), "latest version should be preserved")

    t.Log("GC worker test passed")
}
```

### Design

After GC completes, add a read at a timestamp that would have returned the old version (e.g., `Version: 25`, which is after commitTS 20 but before commitTS 40). After GC with safe point 35, the old version (commitTS 20) should have been removed. A `KvGet` at `Version: 25` should now return `NotFound` (the old write record is gone).

Similarly, update `TestGCWorkerMultipleKeys` to verify old versions are gone for both keys.

### Implementation Steps

1. **Update `TestGCWorkerCleansOldVersions`** (`gc_worker_test.go:95-101`) -- add old version check before the log line:

```go
    // The latest version should still be readable.
    getResp, err := client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-key"), Version: 50})
    require.NoError(t, err)
    assert.False(t, getResp.GetNotFound())
    assert.Equal(t, []byte("new-val"), getResp.GetValue(), "latest version should be preserved")

    // The old version (commitTS=20) should have been garbage collected.
    // Reading at version 25 (after old commit but before new commit) should find nothing.
    getResp, err = client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-key"), Version: 25})
    require.NoError(t, err)
    assert.True(t, getResp.GetNotFound(), "old version should have been garbage collected")

    t.Log("GC worker test passed")
```

2. **Update `TestGCWorkerMultipleKeys`** (`gc_worker_test.go:161-170`) -- add old version checks:

```go
    // Both keys should return new values.
    getResp, err := client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-multi-a"), Version: 50})
    require.NoError(t, err)
    assert.Equal(t, []byte("a-new"), getResp.GetValue())

    getResp, err = client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-multi-b"), Version: 50})
    require.NoError(t, err)
    assert.Equal(t, []byte("b-new"), getResp.GetValue())

    // Old versions (commitTS=20) should have been garbage collected.
    getResp, err = client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-multi-a"), Version: 25})
    require.NoError(t, err)
    assert.True(t, getResp.GetNotFound(), "old version of gc-multi-a should have been garbage collected")

    getResp, err = client.KvGet(ctx, &kvrpcpb.GetRequest{Key: []byte("gc-multi-b"), Version: 25})
    require.NoError(t, err)
    assert.True(t, getResp.GetNotFound(), "old version of gc-multi-b should have been garbage collected")

    t.Log("GC worker multi-key test passed")
```

### Test Plan

1. Run `go test ./e2e/ -run TestGCWorkerCleansOldVersions -v` -- should pass if GC actually deletes old versions.
2. Run `go test ./e2e/ -run TestGCWorkerMultipleKeys -v` -- same verification for multi-key scenario.
3. **Negative validation**: Temporarily disable GC execution (e.g., comment out the GC logic) and verify the new assertions **fail**, confirming they actually test GC behavior.

---

## Issue 2: Test 5.1 -- Duplicate Cluster Helper Functions in e2e_external

### Problem

Two separate files define nearly identical cluster setup helpers:

**`e2e_external/cluster_raw_kv_test.go:16-33`** defines `newClusterWithLeader()`:

```go
func newClusterWithLeader(t *testing.T) *e2elib.GokvCluster {
    t.Helper()
    e2elib.SkipIfNoBinary(t, "gookv-server", "gookv-pd")

    cluster := e2elib.NewGokvCluster(t, e2elib.GokvClusterConfig{NumNodes: 3})
    require.NoError(t, cluster.Start())
    t.Cleanup(func() { cluster.Stop() })

    rawKV := cluster.RawKV()
    e2elib.WaitForCondition(t, 30*time.Second, "cluster leader election", func() bool {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        return rawKV.Put(ctx, []byte("__health__"), []byte("ok")) == nil
    })

    return cluster
}
```

**`e2e_external/client_lib_test.go:17-36`** defines `newClientCluster()`:

```go
func newClientCluster(t *testing.T) (*e2elib.GokvCluster, *client.RawKVClient) {
    t.Helper()
    e2elib.SkipIfNoBinary(t, "gookv-server", "gookv-pd")

    cluster := e2elib.NewGokvCluster(t, e2elib.GokvClusterConfig{NumNodes: 3})
    require.NoError(t, cluster.Start())
    t.Cleanup(func() { cluster.Stop() })

    rawKV := cluster.RawKV()

    e2elib.WaitForCondition(t, 30*time.Second, "cluster leader election", func() bool {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        err := rawKV.Put(ctx, []byte("__health__"), []byte("ok"))
        return err == nil
    })

    return cluster, rawKV
}
```

These are functionally identical -- `newClientCluster` is just `newClusterWithLeader` plus returning the `RawKVClient`. This duplication means any fix to the cluster setup pattern (e.g., changing timeout, health check key, or node count) must be applied in two places.

### Current Code

The two functions are used by different test files:

- `newClusterWithLeader()` is used in: `cluster_raw_kv_test.go` (lines 37, 64)
- `newClientCluster()` is used in: `client_lib_test.go` (lines 41, 54, 79, 95, 117, 141, 166, 183, 204)

Both are in the `e2e_external_test` package, so they are already visible to all test files in the package.

### Design

1. Move `newClusterWithLeader()` to a shared `e2e_external/helpers_test.go` file.
2. Rewrite `newClientCluster()` to call `newClusterWithLeader()` and add the `RawKVClient` return.
3. Remove the duplicate definition from `cluster_raw_kv_test.go`.

### Implementation Steps

1. **Create `e2e_external/helpers_test.go`**:

```go
package e2e_external_test

import (
    "context"
    "testing"
    "time"

    "github.com/stretchr/testify/require"

    "github.com/ryogrid/gookv/pkg/client"
    "github.com/ryogrid/gookv/pkg/e2elib"
)

// newClusterWithLeader creates a 3-node cluster and waits for Raft leader election.
func newClusterWithLeader(t *testing.T) *e2elib.GokvCluster {
    t.Helper()
    e2elib.SkipIfNoBinary(t, "gookv-server", "gookv-pd")

    cluster := e2elib.NewGokvCluster(t, e2elib.GokvClusterConfig{NumNodes: 3})
    require.NoError(t, cluster.Start())
    t.Cleanup(func() { cluster.Stop() })

    // Wait for Raft leader election.
    rawKV := cluster.RawKV()
    e2elib.WaitForCondition(t, 30*time.Second, "cluster leader election", func() bool {
        ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
        defer cancel()
        return rawKV.Put(ctx, []byte("__health__"), []byte("ok")) == nil
    })

    return cluster
}

// newClientCluster creates a 3-node cluster and returns both the cluster and a RawKVClient.
// It waits for Raft leader election before returning.
func newClientCluster(t *testing.T) (*e2elib.GokvCluster, *client.RawKVClient) {
    t.Helper()
    cluster := newClusterWithLeader(t)
    return cluster, cluster.RawKV()
}
```

2. **Remove `newClusterWithLeader()` from `cluster_raw_kv_test.go`** (delete lines 16-33).

3. **Remove `newClientCluster()` from `client_lib_test.go`** (delete lines 17-36).

4. **Remove unused imports from `cluster_raw_kv_test.go`**: After removing the helper, check if `"context"`, `"time"`, and `"github.com/stretchr/testify/require"` are still needed (they likely are for other test functions, but verify).

5. **Remove unused imports from `client_lib_test.go`**: After removing the helper, `"context"`, `"time"`, `require`, and `e2elib` may still be needed by test functions. The `client` import is no longer needed in the helper (it's in `helpers_test.go` now) but is likely still used by test functions. Verify.

### Verification

Check all callers of both functions to ensure they compile correctly:

- `cluster_raw_kv_test.go:37` -- `newClusterWithLeader(t)` -- still works (function moved to same package).
- `cluster_raw_kv_test.go:64` -- `newClusterWithLeader(t)` -- same.
- `client_lib_test.go:41,54,79,95,117,141,166,183,204` -- `newClientCluster(t)` -- same.

Both functions remain in the `e2e_external_test` package, so no import changes are needed in caller files.

### Test Plan

1. **Build verification**: `go test -c ./e2e_external/` compiles successfully.
2. **Run all e2e_external tests**: `go test ./e2e_external/ -v -count=1` -- all tests pass.
3. **Verify no duplicate definitions**: `grep -rn "func newClusterWithLeader\|func newClientCluster" e2e_external/` returns exactly one definition each, both in `helpers_test.go`.

---

## Files Modified

| File | Lines | Change |
|------|-------|--------|
| `e2e/gc_worker_test.go` | 95-101, 161-170 | Add assertions that old versions are `NotFound` after GC |
| `e2e_external/helpers_test.go` | (new file) | Shared `newClusterWithLeader()` and `newClientCluster()` helpers |
| `e2e_external/cluster_raw_kv_test.go` | 16-33 | Remove `newClusterWithLeader()` definition |
| `e2e_external/client_lib_test.go` | 17-36 | Remove `newClientCluster()` definition |

## Risk Assessment

- **Test 1.1 (GC assertion)**: No risk to production code. The new assertions may initially fail if GC has a bug -- that's the point. The test is additive.
- **Test 5.1 (helper consolidation)**: No risk. Pure refactoring of test code. Both functions are in the same Go test package and remain accessible to all test files.

---

## Addendum: Review Feedback Incorporated

**Review verdict: PASS -- no blocking issues.**

Both issues in this design document passed review:

- **Test 1.1 (GC Old Version Check)**: The analysis is accurate. The proposed fix correctly adds assertions that old versions are `NotFound` after GC. The negative validation suggestion (disabling GC to verify assertions fail) is good practice and should be performed during implementation.

- **Test 5.1 (Duplicate Cluster Helpers)**: The consolidation plan is clean. One minor note from review: `cluster.RawKV()` is called in both `newClusterWithLeader` (for the health check) and again in `newClientCluster` (for the return value). If `cluster.RawKV()` creates a new client each time rather than returning a cached instance, the returned `RawKVClient` in `newClientCluster` would be a different instance. This is benign (both connect to the same cluster) but worth verifying during implementation. After refactoring, run `goimports` or `go build` to verify imports are correct in all affected files.
