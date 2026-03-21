# PD Server Raft Replication — Implementation Tracking

## Phase 1: PD Raft Core (~800 LOC)

- [x] Step 1: PD Command Encoding (`internal/pd/command.go` + tests)
- [x] Step 2: PD Raft Storage (`internal/pd/raft_storage.go` + tests)
- [x] Step 3: PD Raft Peer (`internal/pd/raft_peer.go` + tests)
- [x] Step 4: Apply Function (`internal/pd/apply.go` + tests)

## Phase 2: PD-to-PD Transport (~300 LOC)

- [x] Step 5: PD Peer gRPC Service (`internal/pd/peer_service.go`)
- [x] Step 6: PD Transport Client (`internal/pd/transport.go`)
- [x] Step 7: Wire Transport into PDRaftPeer

## Phase 3: Server Integration (~700 LOC)

- [x] Step 8: Embed Raft in PDServer (modify `internal/pd/server.go`)
- [x] Step 8a: TSO and ID Pre-allocation Buffers (`tso_buffer.go`, `id_buffer.go` + tests)
- [x] Step 9: Convert RPC Handlers to Raft-Aware
- [x] Step 10: Leader Forwarding (`internal/pd/forward.go`)
- [x] Step 11: Enhanced GetMembers

## Phase 4: Snapshot & Recovery (~300 LOC)

- [x] Step 12: Snapshot Generation (`internal/pd/snapshot.go`)
- [x] Step 13: Snapshot Application
- [x] Step 14: Recovery on Restart

## Phase 5: CLI & Configuration (~200 LOC)

- [x] Step 15: New CLI Flags (modify `cmd/gookv-pd/main.go`)
- [x] Step 16: PDRaftConfig Construction
- [x] Step 17: Backward-Compatible Startup

## Tests

- [x] Unit tests: command_test.go
- [x] Unit tests: raft_storage_test.go
- [x] Unit tests: raft_peer_test.go
- [x] Unit tests: apply_test.go
- [x] Unit tests: tso_buffer_test.go
- [x] Unit tests: id_buffer_test.go
- [x] Unit tests: snapshot_test.go
- [x] E2E tests: e2e/pd_replication_test.go

## Final Verification

- [x] go vet passes
- [x] All unit tests pass
- [x] All e2e tests pass (including existing)
- [x] TODO.md fully checked
- [x] No TODO/FIXME/HACK/XXX comments in new files
- [x] Deferred items reported (see below)

## Deferred Items

1. **Streaming RPC forwarding (Tso, RegionHeartbeat)**: Followers return `codes.Unavailable` instead of full bidirectional proxy forwarding. The pdclient retry logic handles reconnection to the leader. Full proxy forwarding is a future optimization.
2. **TSOBuffer/IDBuffer integration into RPC handlers**: The buffers are implemented but not yet wired into the Tso/AllocID RPC handlers (which currently propose per-request through Raft). Wiring them is a performance optimization for high-throughput scenarios.
3. **Raft log compaction for PD**: No automatic Raft log compaction is implemented for the PD Raft group. Over time, the Raft log will grow unbounded. A periodic compaction ticker (similar to `onRaftLogGCTick` in raftstore) should be added.
