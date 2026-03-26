# Cross-Region 2PC Integrity Fix

## Production Code

- [x] 1.1 ProposeModifies: add variadic epoch param
- [x] 1.2 ProposeModifies: check Version + ConfVer against current epoch
- [x] 1.3 ProposeModifies: embed current epoch in RaftCmdRequest.Header
- [x] 1.4 applyEntriesForPeer: epoch-aware filtering (match → apply all; mismatch → filter by range)
- [x] 1.5 Extract filterModifiesByRegion helper
- [x] 1.6 proposeErrorToRegionError: add "epoch not match" → EpochNotMatch
- [x] 1.7 Pass epoch from KvPrewrite (standard path)
- [x] 1.8 Pass epoch from KvPrewrite (async commit path)
- [x] 1.9 Pass epoch from KvPrewrite (1PC path)
- [x] 1.10 Pass epoch from KvCommit
- [x] 1.11 Pass epoch from KvBatchRollback, KvCleanup, KvCheckTxnStatus, KvPessimisticLock, KvResolveLock
- [x] 1.12 Update proposeModifiesToRegions/WithRegionError to accept epoch

## Verification

- [x] 2.1 go vet clean
- [x] 2.2 make test — pass
- [x] 2.3 make test-e2e — pass
- [x] 2.4 Final: TODO.md all checked, no stale TODO comments

## Deferred

- Splits as Raft admin commands (long-term TiKV-faithful fix) — out of scope
