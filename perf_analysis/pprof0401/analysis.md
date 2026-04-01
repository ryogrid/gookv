# gookv Transaction Throughput Bottleneck Analysis

**Date**: 2026-04-01  
**Test**: `TestFuzzCluster` with 500 iterations, 4 clients, 5 nodes, 3 regions  
**Profile collection**: During chaos phase (transfers + fault injection active)

## Executive Summary

Server-side CPU utilization is extremely low (**< 0.2% across all nodes**). The transaction throughput bottleneck is **not CPU, memory, or lock contention on the server side**. The primary bottleneck is the **latency of each transaction's Raft consensus round-trips** combined with the **sequential nature of client transaction execution**.

## CPU Profiles (30-second samples)

| Node | Total CPU samples | Top functions |
|------|------------------|---------------|
| PD (10201) | 50ms / 30s (0.17%) | `syscall.Syscall6` (epoll), gRPC framing, HTTP/2 transport |
| KVS 10205 | ~10ms / 30s | gRPC proto unmarshal |
| KVS 10207 | 20ms / 30s (0.07%) | Pebble disk health tick, `epollWait` |
| KVS 10211 | 40ms / 30s (0.13%) | `runtime.futex` (50%), gRPC proto unmarshal (25%), syscall (25%) |

**Key finding**: All servers spend >99.8% of their time **idle/waiting**. The dominant functions are:
- `runtime.futex` — goroutines blocked waiting for events
- `syscall.Syscall6` / `epollWait` — network I/O polling
- gRPC framing/proto unmarshal — Raft message processing

No application-level function (storage, MVCC, 2PC) appears in CPU profiles, indicating the workload is **I/O-latency-bound, not compute-bound**.

## Heap Profiles

### KVS nodes (~16-18 MB in-use)

| Allocation source | Size | % |
|-------------------|------|---|
| `runtime.allocm` (goroutine stacks) | 3.5-4.1 MB | 21-22% |
| `grpc/mem.(*sizedBufferPool).Get` | 2.4-3.6 MB | 13-21% |
| `sendRaftMessage` (Raft message buffers) | 2.0 MB | 11% |
| `raftpb.(*Entry).Unmarshal` | 0.5-1.5 MB | 3-8% |
| Protobuf init-time registration | ~2 MB | 11% |
| Pebble WAL writer | 0.5 MB | 3% |
| `codec.DecodeBytes` | 0.5 MB | 3% |

**Key finding**: Memory is dominated by **goroutine stacks and gRPC buffers**, not application data. The ~16-18 MB total is modest. Raft message serialization (`sendRaftMessage`, `Entry.Unmarshal`) is the largest application-specific allocation.

### PD node (~9 MB in-use)

Dominated by runtime (`allocm`, `malg`) and protobuf init-time allocations. No significant application-level heap pressure.

## Goroutine Profile

KVS node 10211: **96 goroutines**, of which **93 are parked** (`runtime.gopark`).

Active goroutine breakdown:
- 16 Pebble table cache release loops (sleeping)
- 9 gRPC/HTTP readers (blocked on I/O)
- 4 Apply worker pool goroutines (waiting for work)
- 3 Raft peer Run loops (1 per region, ticking)
- 3 Raft gRPC stream handlers
- 1 Pebble WAL flusher, 1 cleanup manager, 1 disk health checker
- 1 signal handler, 1 status server listener

**Key finding**: The apply workers (4) are mostly idle, waiting for Raft-committed entries. The bottleneck is upstream — Raft consensus latency limits how fast entries arrive.

## Block & Mutex Profiles

Both profiles are **empty** — no measurable lock contention or synchronization blocking.

## Bottleneck Analysis

### Where time is spent (per transaction)

A single `doTransfer` transaction performs:
1. **Begin** → PD TSO request (1 gRPC round-trip to PD)
2. **Get(account-a)** → KvGet to region leader (1 gRPC + ReadIndex Raft round-trip)
3. **Get(account-b)** → KvGet to region leader (1 gRPC + ReadIndex Raft round-trip)
4. **Set(a), Set(b)** → buffered locally (no RPC)
5. **Commit/Prewrite** → KvPrewrite to each region (1-2 gRPC + Raft propose + commit)
6. **Commit primary** → KvCommit (1 gRPC + Raft propose + commit)
7. **Commit secondaries** → KvCommit (async, but still Raft round-trips)

**Minimum 5-7 Raft consensus round-trips per transaction**, each requiring:
- gRPC call to leader
- Leader proposes to Raft
- Raft replicates to majority (2/3)
- Leader responds

With Raft tick interval of 100ms and election timeout of 1s, the per-transaction latency is dominated by network + Raft consensus, not computation.

### Why CPU is idle

- 4 concurrent clients, each executing transactions sequentially
- Each transaction blocks for ~50-200ms (Raft round-trips)
- At 4 clients × ~5 txn/s = ~20 txn/s cluster-wide
- Each txn generates ~5 Raft entries, processed in <1ms of CPU time
- Result: servers are idle >99% of the time

## Recommendations for Throughput Improvement

### High Impact

1. **Increase client concurrency**: The current 4 clients are not enough to saturate the cluster. With 20-50 concurrent clients, the server pipeline would be better utilized.

2. **Batch TSO requests**: Each `Begin()` makes a separate TSO call to PD. Batching multiple TSO requests into a single RPC would reduce PD round-trips.

3. **Pipeline Raft proposals**: Currently each KvPrewrite/KvCommit waits for Raft commit before responding. Pipelining (respond after propose, before commit) would reduce latency at the cost of more complex error handling.

4. **Async commit for secondaries**: The commit of secondary keys could be fully async (fire-and-forget after primary commit), reducing transaction latency.

### Medium Impact

5. **ReadIndex batching**: Multiple concurrent reads to the same region could share a single ReadIndex round-trip.

6. **gRPC connection pooling**: Reduce per-connection overhead for high-concurrency workloads.

7. **Reduce Raft tick interval**: The current 100ms tick drives heartbeat frequency. A lower tick (e.g., 50ms) would reduce leader election time and ReadIndex latency.

### Low Impact (already efficient)

- Pebble write path (WAL + memtable) is fast
- Protobuf serialization overhead is minimal
- Memory allocation pressure is low
- No lock contention detected

## Raw Profile Locations

All profiles are stored in `perf_analysis/pprof0401/`:
- `*_cpu.pb.gz` — 30-second CPU profiles
- `*_heap.pb.gz` — Heap allocation profiles
- `*_goroutine.pb.gz` — Goroutine stack profiles
- `*_mutex.pb.gz` — Mutex contention profiles
- `*_block.pb.gz` — Block (synchronization) profiles

View with: `go tool pprof perf_analysis/pprof0401/<file>`
