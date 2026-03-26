# Split as Raft Admin Command

## Step 1: split_admin.go (NEW)

- [ ] 1.1 SplitAdminRequest struct
- [ ] 1.2 MarshalSplitAdminRequest / UnmarshalSplitAdminRequest
- [ ] 1.3 IsSplitAdmin(data) bool
- [ ] 1.4 ExecSplitAdmin(peer, req) → SplitRegionResult

## Step 2: peer.go

- [ ] 2.1 Add splitResultCh field + SetSplitResultCh setter
- [ ] 2.2 Add applySplitAdminEntry method
- [ ] 2.3 Detect SplitAdmin in handleReady committed entries loop

## Step 3: coordinator.go

- [ ] 3.1 Add splitResultCh to StoreCoordinator
- [ ] 3.2 Wire splitResultCh in BootstrapRegion
- [ ] 3.3 Rewrite handleSplitCheckResult to propose via Raft
- [ ] 3.4 Add RunSplitResultHandler goroutine
- [ ] 3.5 Start RunSplitResultHandler in coordinator lifecycle

## Step 4: Cleanup

- [ ] 4.1 Remove filterModifiesByRegion (unused)
- [ ] 4.2 Remove unused imports

## Step 5: Tests

- [ ] 5.1 TestMarshalUnmarshalSplitAdminRequest
- [ ] 5.2 TestIsSplitAdmin
- [ ] 5.3 go vet clean
- [ ] 5.4 make test pass
- [ ] 5.5 make test-e2e pass

## Step 6: Demo verification

- [ ] 6.1 32w/1000a run 1: $100,000
- [ ] 6.2 32w/1000a run 2: $100,000
- [ ] 6.3 32w/1000a run 3: $100,000

## Step 7: Final checks

- [ ] 7.1 TODO.md all items checked
- [ ] 7.2 No stale TODO comments in codebase

## Flaky Tests (Separate Task)

(To be populated during test runs)
