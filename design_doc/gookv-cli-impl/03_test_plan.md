# gookv-cli Test Plan

This document defines the complete test strategy for gookv-cli: unit tests (parser, formatter), integration tests (executor with mocks), E2E tests (full cluster), and regression tests for edge cases.

---

## 1. Unit Tests

### 1.1 Parser: Tokenizer

**File:** `cmd/gookv-cli/parser_test.go`
**Function under test:** `Tokenize(stmt string) ([]Token, error)`

| # | Input | Expected Tokens (Raw) | Expected Values (bytes) | Notes |
|---|-------|----------------------|------------------------|-------|
| 1 | `GET mykey` | `["GET", "mykey"]` | `["GET", "mykey"]` | Simple unquoted |
| 2 | `PUT k1 "hello world"` | `["PUT", "k1", "\"hello world\""]` | `["PUT", "k1", "hello world"]` | Quoted value with space |
| 3 | `PUT k1 "hello\"world"` | ... | `["PUT", "k1", "hello\"world"]` | Escaped double-quote inside quoted string |
| 4 | `GET 0xDEADBEEF` | `["GET", "0xDEADBEEF"]` | `["GET", {DEADBEEF}]` | Hex literal, 0x prefix |
| 5 | `GET x'CAFE'` | `["GET", "x'CAFE'"]` | `["GET", {CAFE}]` | Hex literal, x' prefix |
| 6 | `BGET k1 k2 k3` | `["BGET", "k1", "k2", "k3"]` | 4 tokens | Multiple args |
| 7 | `PUT key "val\nue"` | ... | `["PUT", "key", "val\nue"]` | Escape: newline |
| 8 | `PUT key "val\tue"` | ... | `["PUT", "key", "val\tue"]` | Escape: tab |
| 9 | `PUT key ""` | `["PUT", "key", "\"\""]` | `["PUT", "key", ""]` | Empty quoted string |
| 10 | `GET "key with spaces"` | ... | `["GET", "key with spaces"]` | Key containing spaces |
| 11 | `SCAN "" "" LIMIT 10` | 5 tokens | `["SCAN", "", "", "LIMIT", "10"]` | Empty string keys |
| 12 | `GET user:001` | `["GET", "user:001"]` | `["GET", "user:001"]` | Colon in unquoted token |
| 13 | `BEGIN PESSIMISTIC LOCK_TTL 5000` | 4 tokens | All string values | Multiple keywords |
| 14 | `CAS k1 v2 _ NOT_EXIST` | 5 tokens | All string values | Underscore placeholder |
| 15 | `GET 0xCAFE` | `["GET", "0xCAFE"]` | `["GET", {CAFE}]` | Short hex |
| 16 | `GET "unterminated` | error | -- | Unterminated quote |
| 17 | (empty string) | `[]` | -- | Empty input |
| 18 | `  GET   mykey  ` | `["GET", "mykey"]` | -- | Extra whitespace |
| 19 | `PUT k1 "hello\\world"` | ... | `["PUT", "k1", "hello\\world"]` | Escaped backslash |
| 20 | `GET 0xgg` | error | -- | Invalid hex digits |

### 1.2 Parser: Statement Splitter

**File:** `cmd/gookv-cli/parser_test.go`
**Function under test:** `SplitStatements(input string) ([]string, string, bool)`

| # | Input | Expected Stmts | Remainder | Complete |
|---|-------|---------------|-----------|----------|
| 1 | `GET k1;` | `["GET k1"]` | `""` | true |
| 2 | `PUT k1 v1; PUT k2 v2;` | `["PUT k1 v1", " PUT k2 v2"]` | `""` | true |
| 3 | `PUT k1 v1; GET k2` | `["PUT k1 v1"]` | `" GET k2"` | false |
| 4 | `PUT k1 "hello; world";` | `["PUT k1 \"hello; world\""]` | `""` | true |
| 5 | `GET k1` | `[]` | `"GET k1"` | false |
| 6 | `;;;` | `["", "", ""]` | `""` | true |
| 7 | `PUT k1 "v1"; GET k1` | `["PUT k1 \"v1\""]` | `" GET k1"` | false |
| 8 | (empty string) | `[]` | `""` | true |
| 9 | `  ;  ` | `["  "]` | `"  "` | true |
| 10 | `PUT k1 x'AB;CD';` | `["PUT k1 x'AB;CD'"]` | `""` | true |
| 11 | `GET k1;\nPUT k2 v2;` | `["GET k1", "\nPUT k2 v2"]` | `""` | true |
| 12 | `PUT "a;b" "c;d";` | `["PUT \"a;b\" \"c;d\""]` | `""` | true |

### 1.3 Parser: Meta Command Detection

**File:** `cmd/gookv-cli/parser_test.go`
**Function under test:** `IsMetaCommand(line string) bool`

| # | Input | Expected |
|---|-------|----------|
| 1 | `\help` | true |
| 2 | `\h` | true |
| 3 | `\?` | true |
| 4 | `\quit` | true |
| 5 | `\q` | true |
| 6 | `\timing on` | true |
| 7 | `\format hex` | true |
| 8 | `\pagesize 50` | true |
| 9 | `\x` | true |
| 10 | `\t` | true |
| 11 | `GET key` | false |
| 12 | `HELP;` | false |
| 13 | `\\escaped` | true (starts with `\`) |
| 14 | (empty string) | false |

### 1.4 Parser: Command Parser

**File:** `cmd/gookv-cli/parser_test.go`
**Function under test:** `ParseCommand(tokens []Token, inTxn bool) (Command, error)`

Each test case provides pre-tokenized input (or tokenizes a string and feeds the result).

| # | Tokens (simplified) | inTxn | Expected Type | Expected Args/IntArg | Notes |
|---|---------------------|-------|---------------|---------------------|-------|
| 1 | `GET mykey` | false | `CmdGet` | Args=[mykey] | Raw GET |
| 2 | `GET mykey` | true | `CmdTxnGet` | Args=[mykey] | Txn GET |
| 3 | `PUT k1 v1` | false | `CmdPut` | Args=[k1,v1] | |
| 4 | `PUT k1 v1 TTL 60` | false | `CmdPutTTL` | Args=[k1,v1] IntArg=60 | |
| 5 | `PUT k1 v1 TTL abc` | false | error | | Non-numeric TTL |
| 6 | `DELETE k1` | false | `CmdDelete` | Args=[k1] | |
| 7 | `DELETE k1` | true | `CmdTxnDelete` | Args=[k1] | |
| 8 | `DELETE RANGE a z` | false | `CmdDeleteRange` | Args=[a,z] | |
| 9 | `DELETE RANGE a` | false | error | | Missing end key |
| 10 | `SET k1 v1` | true | `CmdTxnSet` | Args=[k1,v1] | |
| 11 | `SET k1 v1` | false | error | | SET outside txn |
| 12 | `TTL mykey` | false | `CmdTTL` | Args=[mykey] | |
| 13 | `SCAN a z` | false | `CmdScan` | Args=[a,z] IntArg=0 | Default limit |
| 14 | `SCAN a z LIMIT 50` | false | `CmdScan` | Args=[a,z] IntArg=50 | Explicit limit |
| 15 | `SCAN a z LIMIT` | false | error | | Missing limit value |
| 16 | `BGET k1 k2 k3` | false | `CmdBatchGet` | Args=[k1,k2,k3] | |
| 17 | `BGET k1 k2` | true | `CmdTxnBatchGet` | Args=[k1,k2] | |
| 18 | `BPUT k1 v1 k2 v2` | false | `CmdBatchPut` | Args=[k1,v1,k2,v2] | |
| 19 | `BPUT k1 v1 k2` | false | error | | Odd number of args |
| 20 | `BDELETE k1 k2` | false | `CmdBatchDelete` | Args=[k1,k2] | |
| 21 | `BDELETE` | false | error | | No args |
| 22 | `CAS k1 v2 v1` | false | `CmdCAS` | Args=[k1,v2,v1] Flags=0 | Normal CAS |
| 23 | `CAS k1 v2 _ NOT_EXIST` | false | `CmdCAS` | Args=[k1,v2,_] Flags=1 | NOT_EXIST flag |
| 24 | `CAS k1` | false | error | | Too few args |
| 25 | `CHECKSUM` | false | `CmdChecksum` | Args=[] | Full keyspace |
| 26 | `CHECKSUM a z` | false | `CmdChecksum` | Args=[a,z] | Range |
| 27 | `CHECKSUM a` | false | error | | Invalid: 1 arg |
| 28 | `BEGIN` | false | `CmdBegin` | TxnOpts=[] | |
| 29 | `BEGIN PESSIMISTIC` | false | `CmdBegin` | TxnOpts=[Pessimistic] | |
| 30 | `BEGIN ASYNC_COMMIT ONE_PC LOCK_TTL 5000` | false | `CmdBegin` | 3 opts | Combined options |
| 31 | `BEGIN UNKNOWN` | false | error | | Unknown option |
| 32 | `BEGIN LOCK_TTL` | false | error | | Missing TTL value |
| 33 | `COMMIT` | true | `CmdCommit` | | |
| 34 | `ROLLBACK` | true | `CmdRollback` | | |
| 35 | `STORE LIST` | false | `CmdStoreList` | | |
| 36 | `STORE STATUS 1` | false | `CmdStoreStatus` | IntArg=1 | |
| 37 | `STORE STATUS abc` | false | error | | Non-numeric ID |
| 38 | `STORE` | false | error | | Missing sub-command |
| 39 | `REGION mykey` | false | `CmdRegion` | Args=[mykey] | |
| 40 | `REGION LIST` | false | `CmdRegionList` | IntArg=0 | Default limit |
| 41 | `REGION LIST LIMIT 10` | false | `CmdRegionList` | IntArg=10 | |
| 42 | `REGION ID 42` | false | `CmdRegionByID` | IntArg=42 | |
| 43 | `REGION` | false | error | | Missing arg |
| 44 | `CLUSTER INFO` | false | `CmdClusterInfo` | | |
| 45 | `CLUSTER` | false | error | | Missing sub-command |
| 46 | `TSO` | false | `CmdTSO` | | |
| 47 | `GC SAFEPOINT` | false | `CmdGCSafepoint` | | |
| 48 | `GC` | false | error | | Missing sub-command |
| 49 | `STATUS` | false | `CmdStatus` | StrArg="" | |
| 50 | `STATUS 127.0.0.1:20180` | false | `CmdStatus` | StrArg="..." | |
| 51 | `HELP` | false | `CmdHelp` | | |
| 52 | `EXIT` | false | `CmdExit` | | |
| 53 | `QUIT` | false | `CmdExit` | | |
| 54 | `get mykey` | false | `CmdGet` | | Case insensitive |
| 55 | `Scan A Z limit 5` | false | `CmdScan` | IntArg=5 | Mixed case |
| 56 | (empty) | false | error | | Empty statement |
| 57 | `FOOBAR` | false | error | | Unknown command |
| 58 | `GET` | false | error | | Missing key |
| 59 | `PUT k1` | false | error | | Missing value |

### 1.5 Parser: Meta Command Parser

**File:** `cmd/gookv-cli/parser_test.go`
**Function under test:** `ParseMetaCommand(line string) (Command, error)`

| # | Input | Expected Type | StrArg/IntArg | Notes |
|---|-------|---------------|---------------|-------|
| 1 | `\help` | `CmdHelp` | | |
| 2 | `\h` | `CmdHelp` | | Short form |
| 3 | `\?` | `CmdHelp` | | Alt form |
| 4 | `\quit` | `CmdExit` | | |
| 5 | `\q` | `CmdExit` | | Short form |
| 6 | `\timing on` | `CmdMetaTiming` | StrArg="on" | |
| 7 | `\timing off` | `CmdMetaTiming` | StrArg="off" | |
| 8 | `\timing` | error or toggle | | No argument -- toggle |
| 9 | `\t` | `CmdMetaTiming` | StrArg="" (toggle) | |
| 10 | `\format hex` | `CmdMetaFormat` | StrArg="hex" | |
| 11 | `\format table` | `CmdMetaFormat` | StrArg="table" | |
| 12 | `\format plain` | `CmdMetaFormat` | StrArg="plain" | |
| 13 | `\format invalid` | error | | Invalid format |
| 14 | `\pagesize 50` | `CmdMetaPagesize` | IntArg=50 | |
| 15 | `\pagesize abc` | error | | Non-numeric |
| 16 | `\pagesize 0` | error | | Zero page size |
| 17 | `\x` | `CmdMetaHex` | | Cycle mode |
| 18 | `\unknown` | error | | Unknown meta command |

### 1.6 Formatter

**File:** `cmd/gookv-cli/formatter_test.go`

All tests write to a `bytes.Buffer` and assert on the output string.

#### Table Format Tests (5 cases)

| # | Result | Expected Output Contains |
|---|--------|-------------------------|
| 1 | `ResultOK` (Message="") | `"OK\n"` |
| 2 | `ResultOK` (Message="OK (3 pairs)") | `"OK (3 pairs)\n"` |
| 3 | `ResultValue` (Value=[]byte("hello")) | table with `"hello"` in value column, `+---` borders |
| 4 | `ResultRows` (3 pairs) | table with Key/Value headers, 3 data rows, `+---` borders |
| 5 | `ResultTable` (3 cols, 2 rows) | table with custom headers, 2 data rows |

#### Plain Format Tests (5 cases)

| # | Result | Expected Output |
|---|--------|-----------------|
| 1 | `ResultOK` | `"OK\n"` |
| 2 | `ResultValue` ("hello") | `"hello\n"` |
| 3 | `ResultNotFound` | `"(not found)\n"` |
| 4 | `ResultNil` | `"(nil)\n"` |
| 5 | `ResultRows` (2 pairs: k1/v1, k2/v2) | `"k1\tv1\nk2\tv2\n"` |

#### Hex Format Tests (5 cases)

| # | Result | Expected Output |
|---|--------|-----------------|
| 1 | `ResultValue` ("hello") | `"68656c6c6f\n"` |
| 2 | `ResultValue` ([]byte{0x00, 0x01, 0xff}) | `"0001ff\n"` |
| 3 | `ResultRows` (k="a", v="b") | `"61\t62\n"` |
| 4 | `formatBytes` DisplayAuto, printable "abc" | `"abc"` |
| 5 | `formatBytes` DisplayAuto, binary {0x00, 0xff} | `"\x00\xff"` |

#### Timing Tests (2 cases)

| # | showTiming | Expected |
|---|------------|----------|
| 1 | true | output ends with timing like `(1.5ms)` |
| 2 | false | no timing string in output |

#### Special Result Tests (5 cases)

| # | Result | Expected Contains |
|---|--------|-------------------|
| 1 | `ResultCAS` swapped=true, prev="old" | `"OK (swapped)"`, `"previous: \"old\""` |
| 2 | `ResultCAS` swapped=false, prev="cur" | `"FAILED (not swapped)"`, `"previous: \"cur\""` |
| 3 | `ResultCAS` swapped=true, prevNotFound | `"OK (swapped)"`, `"previous: (not found)"` |
| 4 | `ResultChecksum` (crc=0xABC, kvs=100, bytes=5000) | `"Checksum:"`, `"Total keys:"`, `"Total bytes:"` |
| 5 | `ResultScalar` ("TTL: 3600s") | `"TTL: 3600s\n"` |

#### Row Count in Timing (3 cases)

| # | ResultType | rows | notFound | Expected Timing |
|---|------------|------|----------|----------------|
| 1 | ResultRows | 5 | 0 | `"(5 rows, N.Nms)"` |
| 2 | ResultRows | 3 | 2 | `"(3 rows, 2 not found, N.Nms)"` |
| 3 | ResultRows | 0 | 0 | `"(0 rows, N.Nms)"` |

---

## 2. Integration Tests

### 2.1 Executor: Raw KV (Mock-based)

**File:** `cmd/gookv-cli/executor_test.go`

Define a `mockRawKV` struct implementing the `rawKVAPI` interface. Each test configures the mock's return values and verifies the executor produces the correct `Result`.

| # | Test Function | Command | Mock Behavior | Expected Result |
|---|---------------|---------|---------------|-----------------|
| 1 | `TestExecRawGetFound` | `CmdGet` key="k1" | Returns ("v1", false, nil) | `ResultValue` Value="v1" |
| 2 | `TestExecRawGetNotFound` | `CmdGet` key="k1" | Returns (nil, true, nil) | `ResultNotFound` |
| 3 | `TestExecRawGetError` | `CmdGet` key="k1" | Returns (nil, false, err) | error |
| 4 | `TestExecRawPut` | `CmdPut` key="k1" val="v1" | Returns nil | `ResultOK` |
| 5 | `TestExecRawPutError` | `CmdPut` key="k1" val="v1" | Returns err | error |
| 6 | `TestExecRawPutTTL` | `CmdPutTTL` key="k1" val="v1" IntArg=60 | `PutWithTTL` returns nil | `ResultOK` |
| 7 | `TestExecRawDelete` | `CmdDelete` key="k1" | Returns nil | `ResultOK` |
| 8 | `TestExecRawTTLValue` | `CmdTTL` key="k1" | Returns (3600, nil) | `ResultScalar` "TTL: 3600s" |
| 9 | `TestExecRawTTLNoExpiry` | `CmdTTL` key="k1" | Returns (0, nil) | `ResultScalar` "TTL: 0 (no expiration)" |
| 10 | `TestExecRawScan` | `CmdScan` start="a" end="z" limit=10 | Returns 3 pairs | `ResultRows` 3 rows |
| 11 | `TestExecRawScanEmpty` | `CmdScan` start="x" end="y" limit=10 | Returns 0 pairs | `ResultRows` 0 rows |
| 12 | `TestExecRawBatchGet` | `CmdBatchGet` keys=[k1,k2,k3] | Returns 2 pairs (k3 missing) | `ResultRows` 2 rows, NotFoundCount=1 |
| 13 | `TestExecRawBatchPut` | `CmdBatchPut` 3 pairs | Returns nil | `ResultOK` Message="OK (3 pairs)" |
| 14 | `TestExecRawBatchDelete` | `CmdBatchDelete` 2 keys | Returns nil | `ResultOK` Message="OK (2 keys)" |
| 15 | `TestExecRawDeleteRange` | `CmdDeleteRange` start="a" end="z" | Returns nil | `ResultOK` |
| 16 | `TestExecRawCASSwapped` | `CmdCAS` key/new/old | Returns (true, "old", nil) | `ResultCAS` Swapped=true |
| 17 | `TestExecRawCASNotSwapped` | `CmdCAS` key/new/old | Returns (false, "cur", nil) | `ResultCAS` Swapped=false |
| 18 | `TestExecRawCASNotExist` | `CmdCAS` NOT_EXIST flag | `CompareAndSwap(..., true)` called | Verify `prevNotExist=true` passed |
| 19 | `TestExecRawChecksum` | `CmdChecksum` start="a" end="z" | Returns (0xABC, 100, 5000, nil) | `ResultChecksum` |
| 20 | `TestExecRawEmptyKey` | `CmdGet` key="" | -- | error "key must not be empty" |

### 2.2 Executor: Transaction Lifecycle (Mock-based)

**File:** `cmd/gookv-cli/executor_test.go`

Define a `mockTxnKV` implementing `txnKVAPI` and a `mockTxnHandle` (or test with real `TxnHandle` via E2E).

If using interface-based mocks:

```go
type txnHandleAPI interface {
    StartTS() txntypes.TimeStamp
    Get(ctx context.Context, key []byte) ([]byte, error)
    BatchGet(ctx context.Context, keys [][]byte) ([]client.KvPair, error)
    Set(ctx context.Context, key, value []byte) error
    Delete(ctx context.Context, key []byte) error
    Commit(ctx context.Context) error
    Rollback(ctx context.Context) error
}
```

| # | Test Function | Scenario | Expected |
|---|---------------|----------|----------|
| 1 | `TestTxnBeginDefault` | BEGIN (no opts) | `activeTxn` set, `ResultBegin` with startTS |
| 2 | `TestTxnBeginPessimistic` | BEGIN PESSIMISTIC | opts include WithPessimistic |
| 3 | `TestTxnBeginCombinedOpts` | BEGIN ASYNC_COMMIT ONE_PC LOCK_TTL 5000 | 3 opts applied |
| 4 | `TestTxnBeginAlreadyActive` | BEGIN when activeTxn != nil | error "already in progress" |
| 5 | `TestTxnSetSuccess` | SET k1 v1 (txn active) | `ResultOK` |
| 6 | `TestTxnSetNoTxn` | SET k1 v1 (no txn) | error from parser: "only valid inside..." |
| 7 | `TestTxnGetFound` | GET k1 (txn), returns "v1" | `ResultValue` |
| 8 | `TestTxnGetNotFound` | GET k1 (txn), returns nil,nil | `ResultNil` |
| 9 | `TestTxnDeleteSuccess` | DELETE k1 (txn active) | `ResultOK` |
| 10 | `TestTxnBatchGetPartial` | BGET k1 k2 k3, 2 found | `ResultRows`, NotFoundCount=1 |
| 11 | `TestTxnCommitSuccess` | COMMIT | `activeTxn` = nil, `ResultCommit` |
| 12 | `TestTxnCommitFailure` | COMMIT returns error | `activeTxn` stays non-nil |
| 13 | `TestTxnCommitAlreadyCommitted` | COMMIT returns ErrTxnCommitted | `activeTxn` = nil |
| 14 | `TestTxnRollbackSuccess` | ROLLBACK | `activeTxn` = nil, `ResultOK` |
| 15 | `TestTxnRollbackNoTxn` | ROLLBACK (no txn) | error "no active transaction" |
| 16 | `TestTxnCommitNoTxn` | COMMIT (no txn) | error "no active transaction" |
| 17 | `TestTxnFullLifecycle` | BEGIN -> SET -> GET -> COMMIT | All succeed, state transitions correct |
| 18 | `TestTxnRollbackLifecycle` | BEGIN -> SET -> ROLLBACK | activeTxn nil after rollback |

### 2.3 Executor: Admin Commands (MockClient)

**File:** `cmd/gookv-cli/executor_test.go`

Uses `pdclient.NewMockClient()` directly.

| # | Test Function | Setup | Expected |
|---|---------------|-------|----------|
| 1 | `TestAdminStoreList` | Mock: 3 stores registered | `ResultTable` with 3 rows, columns=[StoreID, Address, State] |
| 2 | `TestAdminStoreListEmpty` | Mock: no stores | `ResultTable` with 0 rows |
| 3 | `TestAdminStoreStatus` | Mock: store ID=1 exists | `ResultScalar` with store details |
| 4 | `TestAdminStoreStatusNotFound` | Mock: store ID=99 not registered | error |
| 5 | `TestAdminRegion` | Mock: region with key range | `ResultScalar` with region info |
| 6 | `TestAdminRegionList` | Mock: 3 regions | `ResultTable` with 3 rows |
| 7 | `TestAdminRegionListLimit` | Mock: 5 regions, LIMIT 2 | `ResultTable` with 2 rows |
| 8 | `TestAdminRegionByID` | Mock: region ID=1 exists | `ResultScalar` with region info |
| 9 | `TestAdminRegionByIDNotFound` | Mock: region ID=99 not found | error |
| 10 | `TestAdminClusterInfo` | Mock: 3 stores, 2 regions | Composite output with cluster ID, store table, region count |
| 11 | `TestAdminTSO` | Mock: returns TimeStamp{Physical: 1690000000000, Logical: 0} | `ResultScalar` with decomposition |
| 12 | `TestAdminTSOLogical` | Mock: returns TimeStamp with non-zero Logical | Verify logical component displayed |
| 13 | `TestAdminGCSafepoint` | Mock: returns 443552100000000000 | `ResultScalar` with decomposition |
| 14 | `TestAdminGCSafepointZero` | Mock: returns 0 | `ResultScalar` "0 (not set)" |

### 2.4 Executor: Error Paths

| # | Test Function | Scenario | Expected |
|---|---------------|----------|----------|
| 1 | `TestRawKVDuringTxn` | Execute CmdPut while activeTxn != nil | error "use SET for transactional writes" |
| 2 | `TestTxnCmdOutsideTxn` | Execute CmdCommit without BEGIN | error "no active transaction" |
| 3 | `TestConnectionError` | Mock returns gRPC error | Error propagated with "rpc error:" prefix |
| 4 | `TestRegionError` | Mock returns region error | Error propagated with "region error:" prefix |
| 5 | `TestInvalidCommand` | Execute unknown CommandType | error "unknown command" |

---

## 3. E2E Tests (using e2elib)

**File:** `e2e_external/cli_test.go` (or `e2e_external/cli/cli_test.go`)

E2E tests start a real gookv cluster (PD + KVS nodes) using `pkg/e2elib`, then invoke `gookv-cli` as an external process with `-c` batch mode and verify stdout/stderr/exit code.

### 3.1 Helper: CLI Runner

```go
// runCLI executes gookv-cli with the given arguments and returns stdout, stderr, exit code.
func runCLI(t *testing.T, pdAddr string, args ...string) (stdout, stderr string, exitCode int)

// runCLIBatch is a convenience wrapper for -c batch mode.
func runCLIBatch(t *testing.T, pdAddr, commands string) (stdout, stderr string, exitCode int)
```

### 3.2 Raw KV E2E Tests

| # | Test Function | Commands | Verification |
|---|---------------|----------|-------------|
| 1 | `TestE2ERawPutGet` | `PUT k1 v1; GET k1;` | stdout contains "v1", exit 0 |
| 2 | `TestE2ERawPutTTLGet` | `PUT k1 v1 TTL 60; TTL k1;` | TTL output > 0 |
| 3 | `TestE2ERawDelete` | `PUT k1 v1; DELETE k1; GET k1;` | Final GET shows "(not found)" |
| 4 | `TestE2ERawScan` | `PUT a 1; PUT b 2; PUT c 3; SCAN a d;` | Table with 3 rows |
| 5 | `TestE2ERawScanLimit` | `PUT a 1; PUT b 2; PUT c 3; SCAN a d LIMIT 2;` | Table with 2 rows |
| 6 | `TestE2ERawBatchGet` | `PUT a 1; PUT b 2; BGET a b c;` | Table with 2 rows, "1 not found" in summary |
| 7 | `TestE2ERawBatchPut` | `BPUT a 1 b 2 c 3; BGET a b c;` | "OK (3 pairs)", then 3 rows |
| 8 | `TestE2ERawBatchDelete` | `BPUT a 1 b 2; BDELETE a b; BGET a b;` | "OK (2 keys)", then 0 rows |
| 9 | `TestE2ERawDeleteRange` | `BPUT a 1 b 2 c 3; DELETE RANGE a d; SCAN a d;` | 0 rows after delete range |
| 10 | `TestE2ERawCAS` | `PUT k1 v1; CAS k1 v2 v1;` | "OK (swapped)" |
| 11 | `TestE2ERawCASFail` | `PUT k1 v1; CAS k1 v2 WRONG;` | "FAILED (not swapped)" |
| 12 | `TestE2ERawChecksum` | `BPUT a 1 b 2; CHECKSUM a c;` | Checksum, Total keys: 2 |
| 13 | `TestE2ERawHexKeys` | `PUT 0x414243 0x444546; GET 0x414243;` | Value is "DEF" (or hex) |

### 3.3 Transaction E2E Tests

| # | Test Function | Commands | Verification |
|---|---------------|----------|-------------|
| 1 | `TestE2ETxnRoundTrip` | `BEGIN; SET k1 v1; SET k2 v2; COMMIT; GET k1; GET k2;` | Both GETs return values, exit 0 |
| 2 | `TestE2ETxnRollback` | `PUT k1 before; BEGIN; SET k1 after; ROLLBACK; GET k1;` | GET returns "before" |
| 3 | `TestE2ETxnGetInsideTxn` | `PUT k1 v1; BEGIN; GET k1; SET k1 v2; GET k1; COMMIT;` | First GET="v1", second GET="v2" |
| 4 | `TestE2ETxnPessimistic` | `BEGIN PESSIMISTIC; SET k1 v1; COMMIT; GET k1;` | GET returns "v1" |
| 5 | `TestE2ETxnBatchGet` | `PUT a 1; PUT b 2; BEGIN; BGET a b c; COMMIT;` | 2 rows in BGET output |
| 6 | `TestE2ETxnDeleteInTxn` | `PUT k1 v1; BEGIN; DELETE k1; GET k1; COMMIT; GET k1;` | GET inside txn = "(nil)", GET after = "(not found)" |

### 3.4 Admin E2E Tests

| # | Test Function | Cluster Config | Commands | Verification |
|---|---------------|----------------|----------|-------------|
| 1 | `TestE2EStoreList` | 3 nodes | `STORE LIST;` | Table with 3 rows, all "Up" |
| 2 | `TestE2EStoreStatus` | 3 nodes | `STORE STATUS 1;` | Contains store address |
| 3 | `TestE2ERegion` | 1 region | `REGION "";` | Region ID, StartKey, EndKey |
| 4 | `TestE2ERegionList` | After split | `REGION LIST;` | Multiple regions |
| 5 | `TestE2EClusterInfo` | 3 nodes | `CLUSTER INFO;` | Cluster ID, 3 stores, region count |
| 6 | `TestE2ETSO` | any | `TSO;` | Non-zero timestamp with physical/logical decomposition |
| 7 | `TestE2EGCSafepoint` | any | `GC SAFEPOINT;` | "0 (not set)" or valid safepoint |

### 3.5 Batch Mode and Pipe Mode E2E Tests

| # | Test Function | Method | Verification |
|---|---------------|--------|-------------|
| 1 | `TestE2EBatchFlag` | `-c "PUT k1 v1; GET k1;"` | stdout contains "v1", exit 0 |
| 2 | `TestE2EBatchMultiStatement` | `-c "PUT a 1; PUT b 2; SCAN a c;"` | 2-row scan result |
| 3 | `TestE2EBatchError` | `-c "GET; PUT k1 v1;"` | stderr contains error, exit 1, PUT not executed |
| 4 | `TestE2EPipeMode` | `echo "PUT k1 v1; GET k1;" \| gookv-cli --pd ...` | stdout contains "v1", no prompt characters |
| 5 | `TestE2EVersionFlag` | `--version` | Contains "gookv-cli v", exit 0 |

### 3.6 Meta Command E2E Tests

| # | Test Function | Commands | Verification |
|---|---------------|----------|-------------|
| 1 | `TestE2EHelp` | `-c "HELP;"` | stdout contains "Raw KV Commands:" |
| 2 | `TestE2EExit` | `-c "EXIT;"` | exit 0, no error |

---

## 4. Regression Tests

### 4.1 SET Keyword Collision

**Issue:** `SET` is both a transactional write command and was previously proposed as a meta command prefix (`SET TIMING`, `SET FORMAT`). After the addenda (A6), all meta commands use backslash syntax, eliminating the collision. But we must verify:

| # | Test | Input | Expected |
|---|------|-------|----------|
| 1 | SET outside txn | `SET k1 v1;` (no txn) | error "SET is only valid inside a transaction" |
| 2 | SET inside txn | `BEGIN; SET timing on; COMMIT;` (key="timing", val="on") | Treated as txn SET (key is "timing"), not meta |
| 3 | SET TIMING as txn write | `BEGIN; SET TIMING ON; COMMIT; GET TIMING;` | GET returns "ON" (it's a key-value pair, not a config command) |
| 4 | Meta via backslash | `\timing on` | Timing toggled, not a KV write |

### 4.2 Empty Key/Value

| # | Test | Input | Expected |
|---|------|-------|----------|
| 1 | Empty key GET | `GET "";` | error "key must not be empty" |
| 2 | Empty key PUT | `PUT "" v1;` | error "key must not be empty" |
| 3 | Empty key DELETE | `DELETE "";` | error "key must not be empty" |
| 4 | Empty value PUT | `PUT k1 "";` | Success (empty value is valid) |
| 5 | Empty SCAN range | `SCAN "" "";` | Full keyspace scan (valid) |
| 6 | Empty DELETE RANGE | `DELETE RANGE "" "";` | Full keyspace delete (with confirmation in interactive mode) |

### 4.3 Binary Values (Hex Input/Output)

| # | Test | Input | Expected |
|---|------|-------|----------|
| 1 | Hex key round-trip | `PUT 0x00FF 0xDEAD; GET 0x00FF;` | Value matches |
| 2 | x' syntax | `PUT x'CAFE' x'BABE'; GET x'CAFE';` | Value matches |
| 3 | Mixed hex and string | `PUT 0x414243 hello; GET 0x414243;` | GET returns "hello" |
| 4 | Non-printable output auto | GET key with value `\x00\x01\x02` | Displayed as hex in auto mode |
| 5 | Printable output auto | GET key with value "hello" | Displayed as string in auto mode |
| 6 | Hex mode output | `\x` toggle; GET printable key | Displayed as hex regardless |

### 4.4 Ctrl-C During SCAN (interactive)

| # | Test | Scenario | Expected |
|---|------|----------|----------|
| 1 | Ctrl-C during SCAN | Long-running SCAN with many results | Operation cancelled, REPL continues |
| 2 | Ctrl-C partial input | Type `PUT k1` then Ctrl-C | Buffer discarded, prompt returns to normal |
| 3 | Ctrl-C in txn | BEGIN, then Ctrl-C | Buffer discarded, txn still active |

Note: These are manual/integration tests. Ctrl-C behavior with readline is difficult to test programmatically.

### 4.5 Large Scan Pagination

| # | Test | Scenario | Expected |
|---|------|----------|----------|
| 1 | Default page size | SCAN with 200 keys, no LIMIT | Returns 100 (default pagesize) |
| 2 | Custom page size | `\pagesize 50`, then SCAN | Returns 50 |
| 3 | Explicit LIMIT | SCAN ... LIMIT 10 | Returns 10 regardless of pagesize |
| 4 | LIMIT > available | SCAN with 5 keys, LIMIT 100 | Returns 5 |

### 4.6 Multi-line Input

| # | Test | Input Lines | Expected |
|---|------|-------------|----------|
| 1 | Basic multi-line | `PUT mykey\n  myvalue;` | PUT executes with key="mykey", value="myvalue" |
| 2 | Multi-line in txn | `BEGIN;\nSET k1\n  v1;\nCOMMIT;` | All commands execute |
| 3 | Multi-statement on one line | `PUT k1 v1; PUT k2 v2; GET k1;` | Three commands execute in order |
| 4 | Continuation then Ctrl-C | `PUT k1\n` then Ctrl-C | Buffer discarded, back to `gookv> ` |

### 4.7 Transaction Edge Cases

| # | Test | Scenario | Expected |
|---|------|----------|----------|
| 1 | Double BEGIN | `BEGIN; BEGIN;` | Second BEGIN returns error "already in progress" |
| 2 | Raw PUT in txn | `BEGIN; PUT k1 v1;` | error "use SET for transactional writes" or similar |
| 3 | SCAN in txn | `BEGIN; SCAN a z;` | error: TxnHandle has no Scan method |
| 4 | Commit empty txn | `BEGIN; COMMIT;` | Success (empty commit) |
| 5 | ROLLBACK after COMMIT | `BEGIN; SET k1 v1; COMMIT; ROLLBACK;` | ROLLBACK error "no active transaction" |
| 6 | DELETE RANGE in txn | `BEGIN; DELETE RANGE a z;` | error: DELETE RANGE is raw-only |

### 4.8 Quoting and Special Characters

| # | Test | Input | Expected |
|---|------|-------|----------|
| 1 | Key with colons | `PUT user:001 alice; GET user:001;` | Returns "alice" |
| 2 | Key with dashes | `PUT my-key my-val; GET my-key;` | Returns "my-val" |
| 3 | Quoted semicolons | `PUT k1 "val;ue"; GET k1;` | Returns "val;ue" |
| 4 | Quoted newlines | `PUT k1 "line1\nline2"; GET k1;` | Value contains actual newline |
| 5 | UTF-8 value | `PUT k1 "hello"; GET k1;` | Returns string value |

---

## 5. Test Matrix

Complete command x scenario matrix for all 30+ commands.

### Raw KV Commands (12)

| Command | Normal | Error (args) | Error (server) | Edge Case |
|---------|--------|-------------|----------------|-----------|
| GET | found value | missing key arg | region error | empty key, hex key, non-printable value |
| PUT | success | missing value arg | region error | empty value, TTL=0, very long value |
| PUT TTL | success | non-numeric TTL | region error | TTL=0 (no expiry) |
| DELETE | success | missing key arg | region error | non-existent key (silent success) |
| TTL | value with TTL | missing key arg | region error | no-TTL key returns 0, key not found |
| SCAN | rows found | missing start/end | region error | empty range, full keyspace, LIMIT 0 |
| BGET | partial results | no keys given | region error | all found, none found, 1 key |
| BPUT | success | odd args | region error | 1 pair, large batch |
| BDELETE | success | no keys given | region error | non-existent keys |
| DELETE RANGE | success | missing end key | region error | full keyspace (confirmation) |
| CAS | swapped | 2 args (missing) | region error | NOT_EXIST success/fail, value mismatch |
| CHECKSUM | result | 1 arg (invalid) | region error | full keyspace, empty range |

### Transaction Commands (7)

| Command | Normal | Error (state) | Error (server) | Edge Case |
|---------|--------|--------------|----------------|-----------|
| BEGIN | success | already active | PD unreachable | all option combos |
| SET | success | no txn active | write conflict (pessimistic) | empty value |
| GET (txn) | found | no txn active | lock resolution retry | not found (nil), deleted key |
| DELETE (txn) | success | no txn active | deadlock (pessimistic) | non-existent key |
| BGET (txn) | partial | no txn active | lock resolution | all found, none found |
| COMMIT | success | no txn, already committed | write conflict (optimistic) | empty txn, failure keeps txn |
| ROLLBACK | success | no txn | already committed | double rollback |

### Admin Commands (9)

| Command | Normal | Error (args) | Error (server) | Edge Case |
|---------|--------|-------------|----------------|-----------|
| STORE LIST | stores listed | -- | PD unreachable | empty cluster |
| STORE STATUS | store found | non-numeric ID | PD unreachable | store not found |
| REGION | region found | missing key | PD unreachable | key at boundary |
| REGION LIST | regions listed | -- | PD unreachable | 1 region, LIMIT |
| REGION ID | region found | non-numeric ID | PD unreachable | not found |
| CLUSTER INFO | composite output | -- | PD unreachable | empty cluster |
| TSO | timestamp | -- | PD unreachable | logical > 0 |
| GC SAFEPOINT | non-zero | -- | PD unreachable | zero (not set) |
| STATUS | JSON response | -- | connection refused | default addr |

### Meta Commands (7)

| Command | Normal | Error | Edge Case |
|---------|--------|-------|-----------|
| \help / HELP; | text output | -- | -- |
| \quit / EXIT; | exit | -- | active txn: confirm + rollback |
| \timing on/off/\t | toggle | invalid arg | already in desired state |
| \format table/plain/hex | set format | invalid format | -- |
| \pagesize N | set pagesize | non-numeric, zero, negative | very large N |
| \x | cycle mode | -- | 3 cycles: auto -> hex -> string -> auto |
| QUIT; | exit | -- | same as EXIT |

---

## 6. Test Execution Summary

### By Phase

| Phase | Test Count (approx.) | Dependencies | Run Command |
|-------|---------------------|-------------|-------------|
| Unit: Parser | ~90 cases | None (pure logic) | `go test ./cmd/gookv-cli/ -run TestTokenize\|TestSplit\|TestIsMeta\|TestParse` |
| Unit: Formatter | ~30 cases | tablewriter only | `go test ./cmd/gookv-cli/ -run TestFormatter` |
| Integration: Executor | ~55 cases | Mock interfaces | `go test ./cmd/gookv-cli/ -run TestExec\|TestTxn\|TestAdmin` |
| E2E | ~30 cases | Full cluster | `go test ./e2e_external/cli/... -v -timeout 120s` |
| Regression | ~30 cases | Mixed | Included in unit + E2E |
| **Total** | **~235 cases** | | |

### CI Integration

```makefile
test-cli: build
	go test ./cmd/gookv-cli/... -v -count=1

test-cli-e2e: build
	go test ./e2e_external/cli/... -v -count=1 -timeout 120s
```

---

## Addendum: Review Feedback Incorporated

Resolutions for review findings from `review_notes.md` (2026-03-28). Adds missing
test coverage identified by both reviewers.

### A1. STATUS command test cases (R-10 / TEST-1)

The STATUS command (`CmdStatus`) has parser test coverage (Section 1.4 cases #49,
#50) but is missing executor integration tests and E2E tests. Add the following:

**Executor integration tests** (add to Section 2.3 "Executor: Admin Commands"):

| # | Test Function | Setup | Expected |
|---|---------------|-------|----------|
| 15 | `TestAdminStatusDefaultAddr` | `CmdStatus` with StrArg="" | Uses default address, returns `ResultScalar` with status JSON or connection error |
| 16 | `TestAdminStatusWithAddr` | `CmdStatus` with StrArg="127.0.0.1:20180" | HTTP GET to specified address, returns `ResultScalar` with status JSON |
| 17 | `TestAdminStatusConnectionRefused` | `CmdStatus` with StrArg="127.0.0.1:1" (unused port) | Returns error "connection refused" or similar |

Note: Since STATUS uses HTTP (not gRPC like all other commands), these tests
require either a real HTTP server or an `httptest.Server` mock. The mock approach
is recommended for unit tests: start an `httptest.Server` returning a canned
JSON response, pass its address as `StrArg`.

**E2E test** (add to Section 3.4 "Admin E2E Tests"):

| # | Test Function | Cluster Config | Commands | Verification |
|---|---------------|----------------|----------|-------------|
| 8 | `TestE2EStatus` | 3 nodes | `STATUS;` | stdout contains JSON with version or server info, exit 0 |
| 9 | `TestE2EStatusWithAddr` | 3 nodes | `STATUS 127.0.0.1:20180;` | stdout contains JSON response from the specified node |

### A2. REGION ID E2E test case (R-15 / TEST-2)

The `REGION ID` command has parser tests (Section 1.4 case #42) and mock-based
executor tests (Section 2.3 cases #8-9) but no E2E test. Add the following to
Section 3.4 "Admin E2E Tests":

| # | Test Function | Cluster Config | Commands | Verification |
|---|---------------|----------------|----------|-------------|
| 10 | `TestE2ERegionByID` | default (1 region) | `REGION LIST; REGION ID 1;` | The REGION ID output contains the same region start/end keys as seen in REGION LIST. Verify Region ID, StartKey, and EndKey fields are present. |

The test extracts a region ID from the REGION LIST output (e.g., by parsing the
first row's RegionID column), then verifies `REGION ID <id>` returns matching
region metadata. If the cluster has a single region (the bootstrap region), its
ID is typically 1.

### A3. `CmdGCSafePoint` spelling in test cases (R-7)

Section 1.4 case #47 references `CmdGCSafepoint` (lowercase "p"). The canonical
spelling per `01_detailed_design.md` is `CmdGCSafePoint` (capital "P"). All test
expectations referencing this command type should use `CmdGCSafePoint`.
