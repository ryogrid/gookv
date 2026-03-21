# PD Server Raft Replication — Implementation Tracking

## Phase 1: PD Raft Core (~800 LOC)

- [ ] Step 1: PD Command Encoding (`internal/pd/command.go` + tests)
- [ ] Step 2: PD Raft Storage (`internal/pd/raft_storage.go` + tests)
- [ ] Step 3: PD Raft Peer (`internal/pd/raft_peer.go` + tests)
- [ ] Step 4: Apply Function (`internal/pd/apply.go` + tests)

## Phase 2: PD-to-PD Transport (~300 LOC)

- [ ] Step 5: PD Peer gRPC Service (`internal/pd/peer_service.go`)
- [ ] Step 6: PD Transport Client (`internal/pd/transport.go`)
- [ ] Step 7: Wire Transport into PDRaftPeer

## Phase 3: Server Integration (~700 LOC)

- [ ] Step 8: Embed Raft in PDServer (modify `internal/pd/server.go`)
- [ ] Step 8a: TSO and ID Pre-allocation Buffers (`tso_buffer.go`, `id_buffer.go` + tests)
- [ ] Step 9: Convert RPC Handlers to Raft-Aware
- [ ] Step 10: Leader Forwarding (`internal/pd/forward.go`)
- [ ] Step 11: Enhanced GetMembers

## Phase 4: Snapshot & Recovery (~300 LOC)

- [ ] Step 12: Snapshot Generation (`internal/pd/snapshot.go`)
- [ ] Step 13: Snapshot Application
- [ ] Step 14: Recovery on Restart

## Phase 5: CLI & Configuration (~200 LOC)

- [ ] Step 15: New CLI Flags (modify `cmd/gookv-pd/main.go`)
- [ ] Step 16: PDRaftConfig Construction
- [ ] Step 17: Backward-Compatible Startup

## Tests

- [ ] Unit tests: command_test.go
- [ ] Unit tests: raft_storage_test.go
- [ ] Unit tests: raft_peer_test.go
- [ ] Unit tests: apply_test.go
- [ ] Unit tests: tso_buffer_test.go
- [ ] Unit tests: id_buffer_test.go
- [ ] Unit tests: snapshot_test.go
- [ ] E2E tests: e2e/pd_replication_test.go

## Final Verification

- [ ] go vet passes
- [ ] All unit tests pass
- [ ] All e2e tests pass (including existing)
- [ ] TODO.md fully checked
- [ ] No TODO/FIXME/HACK/XXX comments in new files
- [ ] Deferred items reported
