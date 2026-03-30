# Raft Stability Fix

## Phase 1: KVS Server Changes
- [x] Add `sort` import and `--max-peer-count` flag to `cmd/gookv-server/main.go`
- [x] Extract `bootstrapStoreIDs` helper function
- [x] Fix PD bootstrap call (lines 206-209) to use voter subset
- [x] Fix Raft bootstrap block (lines 282-296) — non-bootstrap nodes skip BootstrapRegion
- [x] Add unit test for `bootstrapStoreIDs`

## Phase 2: e2elib Changes
- [ ] Add `PdHeartbeatInterval` field to `GokvClusterConfig`
- [ ] Update `Start()`: split bootstrap/join nodes, pass `--max-peer-count` via ExtraFlags, add `pd-heartbeat-tick-interval` to TOML
- [ ] Remove `MaxPeerCount = NumNodes` workaround

## Phase 3: Fuzz Test Update
- [ ] Set `PdHeartbeatInterval: "5s"` in fuzz test's GokvClusterConfig
- [ ] Verify `fuzzNodeCount=5` works without workaround

## Phase 4: Build & Test
- [ ] `go vet ./...`
- [ ] `make build`
- [ ] `make test` (unit tests)
- [ ] `make test-e2e-external`
- [ ] `FUZZ_ITERATIONS=50 FUZZ_CLIENTS=4 make test-fuzz-cluster`

## Phase 5: Documentation
- [ ] Update `gookv-design/10_not_yet_implemented.md` — mark #11 resolved
- [ ] Update `current_impl/05_pd.md` — update constraint note
- [ ] Update `USAGE.md` — add `--max-peer-count` to gookv-server flags

## Deferred Items
(none yet)
