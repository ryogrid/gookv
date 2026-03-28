# Performance Optimization Implementation Tracker

## Phase 1: Raft Log Batch Writer (Optimizations 2+3)

### Step 1.1: Create RaftLogWriter
- [x] Define WriteOp, WriteTask structs
- [x] Implement RaftLogWriter with coalescing goroutine (run, processBatch, drainRemaining)
- [x] Add panic recovery per review addendum
- [x] Unit test: TestRaftLogWriter_SingleTask
- [x] Unit test: TestRaftLogWriter_BatchedTasks
- [x] Unit test: TestRaftLogWriter_StopDrainsRemaining

### Step 1.2: Add BuildWriteTask to PeerStorage
- [x] Implement BuildWriteTask() in storage.go
- [x] Implement ApplyWriteTaskPostPersist() in storage.go
- [ ] Unit test: TestBuildWriteTask_MatchesSaveReady

### Step 1.3: Change handleReady to use writer
- [x] Add raftLogWriter field + SetRaftLogWriter() to Peer
- [x] Modify handleReady: conditional writer vs legacy SaveReady
- [x] Peer blocks on task.Done before continuing

### Step 1.4: Wire RaftLogWriter in StoreCoordinator
- [x] Add raftLogWriter field to StoreCoordinator
- [x] Create writer in NewStoreCoordinator when enabled
- [x] Pass writer to peers in BootstrapRegion/CreatePeer
- [x] Shutdown ordering: peers → writer

### Step 1.5: Config flag
- [x] Add EnableBatchRaftWrite + EnableApplyPipeline to RaftStoreConfig
- [x] Wire through main.go → coordinator

### Step 1.6: Phase 1 Verification
- [ ] go vet passes
- [ ] Unit tests pass
- [ ] e2e tests pass
- [ ] e2e_external tests pass (subset)

## Phase 2: Propose-Apply Pipeline (Optimization 1)

### Step 2.1: Create ApplyWorkerPool
- [ ] Define ApplyTask struct
- [ ] Implement ApplyWorkerPool with worker goroutines
- [ ] processTask: apply entries + invoke callbacks + compute appliedIndex
- [ ] Send ApplyResult back to peer mailbox
- [ ] Unit test: TestApplyWorkerPool_ProcessTask
- [ ] Unit test: TestApplyWorkerPool_Shutdown

### Step 2.2: Change handleReady for async apply
- [ ] Add applyWorkerPool field + SetApplyWorkerPool() to Peer
- [ ] Extract applyInline() from current inline logic
- [ ] Implement submitToApplyWorker() — build ApplyTask, submit to pool
- [ ] Conditional: pool != nil → async, else → inline

### Step 2.3: Applied index coordination
- [ ] Add LastCommittedIndex to ApplyTask (per review addendum)
- [ ] Update onApplyResult: SetAppliedIndex, PersistApplyState, sweep pendingReads
- [ ] Handle admin-only batches inline (per review addendum)
- [ ] Add applyInFlight flag for snapshot guard (per review addendum)
- [ ] Add pendingApplyTasks buffer for consecutive Ready batches (per review addendum)

### Step 2.4: Wire ApplyWorkerPool in StoreCoordinator
- [ ] Add EnableApplyPipeline to RaftStoreConfig
- [ ] Create pool in NewStoreCoordinator when enabled
- [ ] Pass pool to peers
- [ ] Shutdown ordering: peers → writer → apply pool

### Step 2.5: Phase 2 Verification
- [ ] go vet passes
- [ ] Unit tests pass
- [ ] e2e tests pass
- [ ] e2e_external restart tests pass
- [ ] txn-integrity-demo passes

## Final Verification
- [ ] TODO.md has no unchecked items
- [ ] No new TODO/FIXME comments in codebase
- [ ] Full test suite passes
