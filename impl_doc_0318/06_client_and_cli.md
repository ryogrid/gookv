# 06. Client and CLI Tools

## 1. Overview

gookv ships two executables:

| Binary | Package | Purpose |
|---|---|---|
| `gookv-server` | `cmd/gookv-server` | Main server daemon -- gRPC API, Raft coordination, HTTP status endpoint |
| `gookv-ctl` | `cmd/gookv-ctl` | Offline admin CLI for inspecting and diagnosing a gookv data directory |

In addition, the public package `pkg/pdclient` provides a reusable Go client library for communicating with the Placement Driver (PD) service.

---

## 2. Server Entry Point (`cmd/gookv-server/main.go`)

### 2.1 CLI Flags

| Flag | Type | Default | Description |
|---|---|---|---|
| `--config` | string | `""` | Path to a TOML configuration file |
| `--addr` | string | (from config) | gRPC listen address; overrides config value |
| `--status-addr` | string | (from config) | HTTP status listen address; overrides config value |
| `--data-dir` | string | (from config) | Storage data directory; overrides config value |
| `--pd-endpoints` | string | (from config) | Comma-separated PD endpoint addresses; overrides config value |
| `--store-id` | uint64 | `0` | Store ID for this node. A non-zero value enables cluster (multi-node Raft) mode |
| `--initial-cluster` | string | `""` | Initial cluster topology in `storeID=addr,storeID=addr,...` format |

### 2.2 Startup Sequence

The `main()` function proceeds through these steps in order:

1. **Flag parsing** -- `flag.Parse()` reads all CLI flags listed above.

2. **Config loading** -- If `--config` is provided, the TOML file is loaded via `config.LoadFromFile()`. Otherwise `config.DefaultConfig()` supplies sensible defaults.

3. **CLI flag overrides** -- Non-empty CLI flags (`--addr`, `--status-addr`, `--data-dir`, `--pd-endpoints`) override corresponding config fields. This allows operators to use a config file as a base and tweak individual settings per invocation.

4. **Config validation** -- `cfg.Validate()` is called; the process exits on error.

5. **Pebble engine open** -- `rocks.Open(cfg.Storage.DataDir)` opens the underlying Pebble storage engine at the configured data directory. The engine is deferred-closed on exit.

6. **Storage layer creation** -- `server.NewStorage(engine)` wraps the raw engine with the storage abstraction used by the gRPC service handlers.

7. **gRPC Server creation** -- `server.NewServer(srvCfg, storage)` creates the gRPC server and registers the TiKV-compatible service (tikvService).

8. **Cluster mode branch** -- If `--store-id > 0` AND `--initial-cluster` is non-empty, cluster (multi-node Raft) mode activates:
   - `parseInitialCluster()` parses the `"storeID=addr,storeID=addr,..."` string into a `map[uint64]string`.
   - `server.NewStaticStoreResolver(clusterMap)` creates a resolver that maps store IDs to network addresses.
   - `transport.NewRaftClient(resolver, config)` creates a Raft transport client for inter-node communication.
   - `raftrouter.New(256)` creates the Raft message router with 256 shards.
   - `server.NewStoreCoordinator(...)` creates the coordinator that ties together the engine, storage, router, and Raft client.
   - The coordinator is attached to the server via `srv.SetCoordinator(coord)`.
   - **Region bootstrap**: A single region (ID=1) is created spanning all stores. Each store ID doubles as a peer ID (`Peer{Id: storeID, StoreId: storeID}`). `coord.BootstrapRegion()` initializes the Raft group.

9. **gRPC server start** -- `srv.Start()` begins accepting gRPC connections.

10. **HTTP status server start** -- `statusserver.New(...)` is configured with a `ConfigFn` that returns the current config, then `statusSrv.Start()` begins serving HTTP diagnostics.

11. **Signal handling** -- The process blocks on a channel listening for `SIGINT` or `SIGTERM`.

12. **Graceful shutdown** -- On signal receipt:
    - `coord.Stop()` (if cluster mode) stops Raft coordination.
    - `statusSrv.Stop()` stops the HTTP status server.
    - `srv.Stop()` stops the gRPC server.
    - The deferred `engine.Close()` flushes and closes the storage engine.

### 2.3 Helper Functions

- **`splitEndpoints(s string) []string`** -- Splits a comma-separated string into trimmed non-empty endpoint addresses.
- **`parseInitialCluster(s string) map[uint64]string`** -- Parses `"1=127.0.0.1:20160,2=127.0.0.1:20161"` into `{1: "127.0.0.1:20160", 2: "127.0.0.1:20161"}`. Silently skips malformed entries.

---

## 3. Admin CLI (`cmd/gookv-ctl/main.go`)

### 3.1 Command Dispatch

The CLI uses positional subcommands. `main()` reads `os.Args[1]` as the command name and dispatches via a switch statement. Unrecognized commands print usage and exit with code 1.

Two exported helper functions support testability:

- **`RunCommand(args []string) int`** -- Executes a command from a string slice (e.g. `["scan", "--db", "/data"]`). Returns 0 on success, 1 on failure or unknown command.
- **`ParseCommand(input string) string`** -- Extracts the command name from a whitespace-delimited string. Used in test harnesses.

### 3.2 Database Access

All commands that need storage call:

```go
func openDB(path string) traits.KvEngine
```

This opens a Pebble database at the given path via `rocks.Open()` and exits on error. The returned engine provides column-family-aware iterators, point reads, and snapshots.

### 3.3 Commands

#### 3.3.1 `scan` -- Range scan within a column family

| Flag | Type | Default | Description |
|---|---|---|---|
| `--db` | string | (required) | Path to data directory |
| `--cf` | string | `"default"` | Column family name (`default`, `lock`, `write`, `raft`) |
| `--start` | string | `""` | Start key in hex encoding (inclusive); empty = beginning |
| `--end` | string | `""` | End key in hex encoding (exclusive); empty = unbounded |
| `--limit` | int | `100` | Maximum number of keys to return |

Creates an iterator with `LowerBound`/`UpperBound` options, seeks to first, and prints each key-value pair. Values are displayed as printable ASCII when possible, otherwise as hex.

#### 3.3.2 `get` -- Single key lookup

| Flag | Type | Default | Description |
|---|---|---|---|
| `--db` | string | (required) | Path to data directory |
| `--cf` | string | `"default"` | Column family name |
| `--key` | string | (required) | Key in hex encoding |

Performs a point read via `eng.Get(cf, key)`. Prints the column family, key (hex), and value. Reports "Key not found" when `ErrNotFound` is returned.

#### 3.3.3 `mvcc` -- MVCC information for a user key

| Flag | Type | Default | Description |
|---|---|---|---|
| `--db` | string | (required) | Path to data directory |
| `--key` | string | (required) | User key in hex encoding |

This command inspects the multi-version concurrency control state for a single user key. It:

1. Opens a snapshot and creates an `mvcc.MvccReader`.
2. **Lock check**: Calls `reader.LoadLock(userKey)` to find any active lock. If present, reports lock type, primary key, start timestamp, TTL, for-update timestamp, min-commit timestamp, async-commit flag, and short value.
3. **Write scan**: Creates an iterator over the `write` column family. Seeks to `mvcc.EncodeKey(userKey, TSMax)` and iterates backward through commit timestamps. For each write record it calls `txntypes.UnmarshalWrite()` and reports commit timestamp, start timestamp, write type (Put/Delete/Lock/Rollback), and short value. Stops after 10 write records.
4. Outputs all information as formatted JSON using three struct types:
   - `MvccInfo` -- top-level container with key, optional lock, and write list.
   - `LockInfo` -- lock details (type, primary, timestamps, TTL, async-commit).
   - `WriteInfo` -- write record details (commit/start timestamps, type, short value).

#### 3.3.4 `dump` -- Raw hex dump

| Flag | Type | Default | Description |
|---|---|---|---|
| `--db` | string | (required) | Path to data directory |
| `--cf` | string | `"default"` | Column family name |
| `--limit` | int | `50` | Maximum entries to dump |

Iterates from the beginning of the specified column family and prints each key-value pair as tab-separated hex strings. Useful for low-level debugging.

#### 3.3.5 `size` -- Approximate data size per column family

| Flag | Type | Default | Description |
|---|---|---|---|
| `--db` | string | (required) | Path to data directory |

Iterates all four column families (`default`, `lock`, `write`, `raft`) and computes:
- Key count per CF
- Total byte size (sum of key + value lengths)

Sizes are formatted with human-readable units (B, KB, MB, GB) via the `formatSize()` helper.

#### 3.3.6 `compact` -- Trigger WAL sync

| Flag | Type | Default | Description |
|---|---|---|---|
| `--db` | string | (required) | Path to data directory |

Calls `eng.SyncWAL()` on the opened engine. Despite its name, this does **not** trigger actual LSM-tree compaction -- it only forces a write-ahead log sync to ensure durability. The misleading "Compaction triggered successfully" message is printed on completion.

#### 3.3.7 `region` -- Region metadata inspection

Listed in the usage text but **not implemented**. The switch statement in `main()` does not have a `"region"` case, so invoking it falls through to the "Unknown command" error path.

### 3.4 Utility Functions

- **`tryPrintable(data []byte) string`** -- Returns the data as a plain string if all bytes are printable ASCII (0x20-0x7E), otherwise returns hex encoding. Used for human-friendly value display.
- **`formatSize(bytes int64) string`** -- Converts byte counts to human-readable format (B/KB/MB/GB).
- **`writeTypeStr(wt txntypes.WriteType) string`** -- Maps write type constants to readable strings: `Put`, `Delete`, `Lock`, `Rollback`, or `Unknown(N)`.

---

## 4. PD Client Library (`pkg/pdclient/`)

The `pdclient` package is a public Go library for interacting with the Placement Driver service. It lives under `pkg/` so external consumers can import it.

### 4.1 Client Interface

The `Client` interface defines 13 methods:

| Method | Purpose |
|---|---|
| `GetTS` | Allocate a globally unique timestamp from PD's TSO |
| `GetRegion` | Look up the region containing a given key, plus its leader |
| `GetRegionByID` | Look up a region by ID |
| `GetStore` | Get store metadata by store ID |
| `Bootstrap` | Bootstrap the cluster with an initial store and region |
| `IsBootstrapped` | Check if the cluster has been bootstrapped |
| `PutStore` | Register or update a store in PD |
| `ReportRegionHeartbeat` | Send region heartbeat; receive scheduling commands |
| `StoreHeartbeat` | Send store-level heartbeat |
| `AskBatchSplit` | Request new region/peer IDs for split operations |
| `ReportBatchSplit` | Notify PD of completed region splits |
| `AllocID` | Allocate a unique ID from PD |
| `GetClusterID` | Return the cluster identifier |
| `Close` | Shut down the client |

### 4.2 Timestamp Encoding

The `TimeStamp` type encodes physical (milliseconds since epoch) and logical components. `ToUint64()` packs them as `physical<<18 | logical`, compatible with TiKV's timestamp format. `TimeStampFromUint64()` decodes back.

### 4.3 gRPC Client Implementation

`grpcClient` connects to PD via gRPC using kvproto's `pdpb.PDClient`. On creation (`NewClient`), it dials the first reachable endpoint, discovers the cluster ID via `GetMembers`, and stores the connection. All RPC methods attach a `RequestHeader` with the cluster ID.

Configuration (`Config`) supports:
- `Endpoints` -- PD server addresses (default: `127.0.0.1:2379`)
- `RetryInterval` -- 300ms default retry interval
- `RetryMaxCount` -- 10 retries by default
- `UpdateInterval` -- 10-minute PD leader refresh interval

### 4.4 Mock Client

`MockClient` is an in-memory implementation of `Client` for testing. It simulates:
- TSO allocation with atomic counters (physical/logical, with logical rollover at 2^18)
- Region and store registries (in-memory maps)
- Batch split ID allocation (starting at ID 1000)
- Region heartbeat processing (stores region/leader updates)

Test helper methods: `SetRegion`, `SetStore`, `SetHeartbeatResponse`, `GetAllRegions`.

---

## 5. Server Startup Flow

```mermaid
flowchart TD
    A[Parse CLI flags] --> B{--config provided?}
    B -- Yes --> C[Load TOML config file]
    B -- No --> D[Use DefaultConfig]
    C --> E[Apply CLI flag overrides]
    D --> E
    E --> F[Validate config]
    F --> G[Open Pebble engine]
    G --> H[Create Storage layer]
    H --> I[Create gRPC Server]
    I --> J{--store-id > 0 AND\n--initial-cluster set?}

    J -- Yes: Cluster Mode --> K[Parse initial-cluster map]
    K --> L[Create StaticStoreResolver]
    L --> M[Create RaftClient]
    M --> N[Create Router 256 shards]
    N --> O[Create StoreCoordinator]
    O --> P[Attach coordinator to server]
    P --> Q[Bootstrap Region 1\nwith all peers]
    Q --> R[Start gRPC server]

    J -- No: Standalone Mode --> R

    R --> S[Start HTTP status server]
    S --> T[Block on SIGINT / SIGTERM]
    T --> U{Coordinator exists?}
    U -- Yes --> V[Stop coordinator]
    V --> W[Stop status server]
    U -- No --> W
    W --> X[Stop gRPC server]
    X --> Y[Close engine]
```

---

## 6. Implementation Status

| Component | Status | Notes |
|---|---|---|
| Server startup (standalone) | Implemented | Config loading, engine, gRPC, status server |
| Server startup (cluster mode) | Implemented | Raft coordination, region bootstrap, graceful shutdown |
| CLI: `scan` | Implemented | Column-family range scan with hex key bounds |
| CLI: `get` | Implemented | Single key point lookup |
| CLI: `mvcc` | Implemented | Lock inspection, write history (up to 10 records), JSON output |
| CLI: `dump` | Implemented | Raw hex key-value dump |
| CLI: `size` | Implemented | Per-CF key count and byte size |
| CLI: `compact` | Partial | Only calls `SyncWAL()`; does not trigger actual LSM compaction |
| CLI: `region` | Not implemented | Listed in usage text but has no handler; falls through to error |
| PD client library | Implemented | Full gRPC client + mock for testing |
