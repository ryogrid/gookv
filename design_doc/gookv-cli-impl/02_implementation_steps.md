# gookv-cli Implementation Steps

Each step produces a buildable, independently testable artifact. Steps are ordered by dependency: later steps build on earlier ones, but each step's tests can run in isolation using mocks or stubs.

**Canonical spec:** All command names, options, and output formats follow the addenda in `design_doc/gookv-cli/01_overview.md` (Sections A1--A8), which supersede any conflicting content in `02_command_reference.md` and `03_implementation_plan.md`.

---

## Step 1: Scaffold `cmd/gookv-cli/main.go`

### Goal
Create the entry point binary with flag parsing, client initialization, PD client accessor, and mode dispatch (batch vs REPL stub).

### Files to Create

**`cmd/gookv-cli/main.go`** (new)

```go
package main

import (
    "context"
    "flag"
    "fmt"
    "os"
    "os/signal"
    "runtime"
    "strings"
    "syscall"

    "github.com/ryogrid/gookv/pkg/client"
    "github.com/ryogrid/gookv/pkg/pdclient"
)

const version = "0.1.0"

func main() {
    pd := flag.String("pd", "127.0.0.1:2379", "PD server address(es), comma-separated")
    batch := flag.String("c", "", "Execute statement(s) and exit")
    showVersion := flag.Bool("version", false, "Print version and exit")
    flag.Parse()

    if *showVersion {
        fmt.Printf("gookv-cli v%s (go%s, %s/%s)\n", version, runtime.Version()[2:], runtime.GOOS, runtime.GOARCH)
        os.Exit(0)
    }

    ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
    defer cancel()

    endpoints := splitEndpoints(*pd)

    c, err := client.NewClient(ctx, client.Config{
        PDAddrs: endpoints,
    })
    if err != nil {
        fmt.Fprintf(os.Stderr, "ERROR: connect to PD: %v\n", err)
        os.Exit(1)
    }
    defer func() { _ = c.Close() }()

    // Separate PD client for admin commands
    pdClient := c.PD()

    exec := NewExecutor(c, pdClient)
    fmtr := NewFormatter(os.Stdout)

    if *batch != "" {
        os.Exit(runBatch(ctx, exec, fmtr, *batch))
    }
    os.Exit(runREPL(ctx, exec, fmtr))
}

func splitEndpoints(s string) []string {
    parts := strings.Split(s, ",")
    var result []string
    for _, p := range parts {
        p = strings.TrimSpace(p)
        if p != "" {
            result = append(result, p)
        }
    }
    return result
}
```

### Files to Modify

**`pkg/client/client.go`** -- Add `PD()` accessor method:

```go
// PD returns the underlying pdclient.Client for administrative operations.
func (c *Client) PD() pdclient.Client {
    return c.pdClient
}
```

This resolves review issue [I-1]/[Impl-2]/[T-3]: the `Client` struct has `pdClient` as an unexported field, and the CLI needs access to it for admin commands (STORE LIST, REGION, etc.).

**`Makefile`** -- Add `gookv-cli` to `build` target:

```makefile
build: gookv-server gookv-ctl gookv-pd gookv-cli

gookv-cli: $(GO_SRC) go.mod go.sum
	go build -o gookv-cli ./cmd/gookv-cli
```

### Stub Files (for compilation)

Create minimal stubs so the binary compiles:

**`cmd/gookv-cli/executor.go`** -- Empty `Executor` struct with `NewExecutor`.

**`cmd/gookv-cli/formatter.go`** -- Empty `Formatter` struct with `NewFormatter`.

**`cmd/gookv-cli/repl.go`** -- Stub `runREPL` returning 0 and `runBatch` returning 0.

### Acceptance Criteria

1. `make build` produces `gookv-cli` binary without errors.
2. `./gookv-cli --version` prints `gookv-cli v0.1.0 (go1.25, linux/amd64)` and exits 0.
3. `./gookv-cli --pd 127.0.0.1:9999` fails with `ERROR: connect to PD: ...` and exits 1 (no PD running).
4. `go vet ./cmd/gookv-cli/...` passes.
5. `pkg/client.Client` has a new `PD() pdclient.Client` method.

---

## Step 2: Implement Tokenizer + Statement Splitter

### Goal
Build the low-level text processing layer: tokenizing input into tokens (handling quoted strings, hex literals, unquoted words) and splitting multi-statement input on semicolons.

### Files to Create

**`cmd/gookv-cli/parser.go`** (new)

#### Types and Functions

```go
// Token represents a parsed input token.
type Token struct {
    Raw   string // original text (for error messages)
    Value []byte // decoded byte value (hex decoded, unquoted, etc.)
    IsHex bool   // true if the token was a hex literal (0x... or x'...')
}

// Tokenize splits a statement string into tokens.
// Handles: double-quoted strings ("..."), hex literals (0x..., x'...'),
// and unquoted whitespace-delimited tokens.
// Returns an error for unterminated quotes.
func Tokenize(stmt string) ([]Token, error)

// SplitStatements splits input on semicolons, respecting quoted strings.
// Returns:
//   - stmts: list of complete (semicolon-terminated) statements with the
//     semicolons stripped
//   - remainder: any trailing text after the last semicolon (incomplete)
//   - complete: true if the input ended with a semicolon (remainder is empty)
func SplitStatements(input string) (stmts []string, remainder string, complete bool)

// IsMetaCommand returns true if the line starts with a backslash command
// (\help, \quit, \timing, \format, \pagesize, \h, \q, \x, \t, \?).
func IsMetaCommand(line string) bool

// DecodeValue interprets a raw string token as bytes:
//   - "0x..." or "x'...'" -> hex decode
//   - "\"...\"" -> unquote (handle \n, \t, \\, \")
//   - otherwise -> UTF-8 bytes
func DecodeValue(raw string) ([]byte, error)
```

#### Parsing Rules
- Semicolons inside double-quoted strings are not statement separators.
- Semicolons inside hex literals (`x'...'`) are not statement separators.
- Backslash commands (`\help`, `\q`, etc.) do not require a semicolon terminator. `IsMetaCommand` detects them so the REPL can execute them immediately without waiting for `;`.
- Escape sequences in double-quoted strings: `\\`, `\"`, `\n`, `\t`.
- Hex literal formats: `0xDEADBEEF` (no separator) and `x'DEADBEEF'` (MySQL-style). Case insensitive for hex digits.
- Unquoted tokens end at whitespace, semicolons, or double-quotes.

### Files to Create

**`cmd/gookv-cli/parser_test.go`** (new)

#### Test Cases for `Tokenize` (15+ cases)

| # | Input | Expected Tokens | Notes |
|---|-------|-----------------|-------|
| 1 | `GET mykey` | `["GET", "mykey"]` | Simple unquoted |
| 2 | `PUT k1 "hello world"` | `["PUT", "k1", "hello world"]` | Quoted value with space |
| 3 | `PUT k1 "hello\"world"` | `["PUT", "k1", "hello\"world"]` | Escaped quote |
| 4 | `GET 0xDEADBEEF` | `["GET", <bytes:DEADBEEF>]` | Hex literal, 0x prefix |
| 5 | `GET x'CAFE'` | `["GET", <bytes:CAFE>]` | Hex literal, x' prefix |
| 6 | `BGET k1 k2 k3` | `["BGET", "k1", "k2", "k3"]` | Multiple args |
| 7 | `PUT key "val\nue"` | `["PUT", "key", "val\nue"]` | Escape: newline |
| 8 | `PUT key "val\tue"` | `["PUT", "key", "val\tue"]` | Escape: tab |
| 9 | `PUT key ""` | `["PUT", "key", ""]` | Empty quoted string |
| 10 | `GET "key with spaces"` | `["GET", "key with spaces"]` | Key with spaces |
| 11 | `PUT key "he said \"hi\""` | `["PUT", "key", "he said \"hi\""]` | Nested escaped quotes |
| 12 | `SCAN "" "" LIMIT 10` | `["SCAN", "", "", "LIMIT", "10"]` | Empty string keys for full range |
| 13 | `GET user:001` | `["GET", "user:001"]` | Colon in unquoted token |
| 14 | `BEGIN PESSIMISTIC LOCK_TTL 5000` | `["BEGIN", "PESSIMISTIC", "LOCK_TTL", "5000"]` | Multiple keywords |
| 15 | `CAS k1 v2 _ NOT_EXIST` | `["CAS", "k1", "v2", "_", "NOT_EXIST"]` | Underscore as placeholder |
| 16 | `GET 0xCAFE` | `["GET", <bytes:CAFE>]` | Short hex |
| 17 | `GET "unterminated` | error: unterminated quote | Error case |

#### Test Cases for `SplitStatements` (10+ cases)

| # | Input | Stmts | Remainder | Complete |
|---|-------|-------|-----------|----------|
| 1 | `GET k1;` | `["GET k1"]` | `""` | true |
| 2 | `PUT k1 v1; PUT k2 v2;` | `["PUT k1 v1", "PUT k2 v2"]` | `""` | true |
| 3 | `PUT k1 v1; GET k2` | `["PUT k1 v1"]` | `"GET k2"` | false |
| 4 | `PUT k1 "hello; world";` | `["PUT k1 \"hello; world\""]` | `""` | true |
| 5 | `GET k1` | `[]` | `"GET k1"` | false |
| 6 | `;;;` | `["", "", ""]` | `""` | true |
| 7 | `PUT k1 "v1"; PUT k2 "v2"; GET k1` | `["PUT k1 \"v1\"", "PUT k2 \"v2\""]` | `"GET k1"` | false |
| 8 | `(empty string)` | `[]` | `""` | true |
| 9 | `  ;  ` | `[""]` | `""` | true |
| 10 | `PUT k1 x'AB;CD';` | `["PUT k1 x'AB;CD'"]` | `""` | true |

#### Test Cases for `IsMetaCommand`

| # | Input | Expected |
|---|-------|----------|
| 1 | `\help` | true |
| 2 | `\q` | true |
| 3 | `\timing on` | true |
| 4 | `\format hex` | true |
| 5 | `\pagesize 50` | true |
| 6 | `GET key` | false |
| 7 | `HELP;` | false (keyword form, requires parser) |

### Acceptance Criteria

1. `go test ./cmd/gookv-cli/ -run TestTokenize` passes all 17 cases.
2. `go test ./cmd/gookv-cli/ -run TestSplitStatements` passes all 10 cases.
3. `go test ./cmd/gookv-cli/ -run TestIsMetaCommand` passes all 7 cases.
4. No dependency on `pkg/client` or network -- pure string processing.

---

## Step 3: Implement Command Parser

### Goal
Parse tokenized input into `Command` structs, handling context-sensitive dispatch (GET/DELETE inside txn vs outside), multi-word commands (DELETE RANGE, STORE LIST, etc.), and argument validation.

### Files to Modify

**`cmd/gookv-cli/parser.go`** -- Add types and `ParseCommand` function.

#### Types

```go
type CommandType int

const (
    // Raw KV
    CmdGet CommandType = iota
    CmdPut
    CmdPutTTL
    CmdDelete
    CmdTTL
    CmdScan
    CmdBatchGet
    CmdBatchPut
    CmdBatchDelete
    CmdDeleteRange
    CmdCAS
    CmdChecksum

    // Transactional (context-sensitive: same CommandType, dispatch in executor)
    CmdTxnGet
    CmdTxnBatchGet
    CmdTxnSet
    CmdTxnDelete
    CmdBegin
    CmdCommit
    CmdRollback

    // Admin
    CmdClusterInfo
    CmdStoreList
    CmdStoreStatus
    CmdRegion
    CmdRegionList
    CmdRegionByID
    CmdTSO
    CmdGCSafepoint
    CmdStatus

    // Meta (keyword forms requiring semicolons)
    CmdHelp
    CmdExit

    // Meta (backslash forms, no semicolons)
    CmdMetaTiming
    CmdMetaFormat
    CmdMetaPagesize
    CmdMetaHex     // \x toggle
)

type Command struct {
    Type    CommandType
    Args    [][]byte         // positional arguments (keys, values) as raw bytes
    IntArg  int64            // numeric argument (limit, TTL, store ID, region ID)
    TxnOpts []client.TxnOption // BEGIN options
    StrArg  string           // string argument (\timing on/off, \format table/plain/hex, STATUS addr)
    Flags   uint32           // bitfield for CAS NOT_EXIST flag, etc.
}

// ParseCommand parses a tokenized statement into a Command.
// inTxn indicates whether a transaction is currently active, which controls
// context-sensitive dispatch of GET, DELETE, and BGET.
func ParseCommand(tokens []Token, inTxn bool) (Command, error)

// ParseMetaCommand parses a backslash meta command line (no semicolons).
// Input is the full line starting with '\'.
func ParseMetaCommand(line string) (Command, error)
```

#### Dispatch Logic

The parser uses the first token (case-insensitive) and optionally a second token to identify the command:

| First Token | Second Token | inTxn | CommandType | Arg Count |
|---|---|---|---|---|
| `GET` | -- | false | `CmdGet` | 1 key |
| `GET` | -- | true | `CmdTxnGet` | 1 key |
| `PUT` | -- | -- | `CmdPut` or `CmdPutTTL` | 2 (key, val) + optional TTL |
| `DELETE` | `RANGE` | -- | `CmdDeleteRange` | 2 keys |
| `DELETE` | (not RANGE) | false | `CmdDelete` | 1 key |
| `DELETE` | (not RANGE) | true | `CmdTxnDelete` | 1 key |
| `SET` | -- | true | `CmdTxnSet` | 2 (key, val) |
| `SET` | -- | false | error: "SET is only valid inside a transaction" |
| `TTL` | -- | -- | `CmdTTL` | 1 key |
| `SCAN` | -- | -- | `CmdScan` | 2 keys + optional LIMIT int |
| `BGET` | -- | false | `CmdBatchGet` | 1+ keys |
| `BGET` | -- | true | `CmdTxnBatchGet` | 1+ keys |
| `BPUT` | -- | -- | `CmdBatchPut` | 2+ (even count) |
| `BDELETE` | -- | -- | `CmdBatchDelete` | 1+ keys |
| `CAS` | -- | -- | `CmdCAS` | 3 (key, new, old) + optional NOT_EXIST |
| `CHECKSUM` | -- | -- | `CmdChecksum` | 0 or 2 keys |
| `BEGIN` | -- | -- | `CmdBegin` | 0+ options |
| `COMMIT` | -- | -- | `CmdCommit` | 0 |
| `ROLLBACK` | -- | -- | `CmdRollback` | 0 |
| `STORE` | `LIST` | -- | `CmdStoreList` | 0 |
| `STORE` | `STATUS` | -- | `CmdStoreStatus` | 1 int (store ID) |
| `REGION` | `LIST` | -- | `CmdRegionList` | optional LIMIT int |
| `REGION` | `ID` | -- | `CmdRegionByID` | 1 int (region ID) |
| `REGION` | (key) | -- | `CmdRegion` | 1 key |
| `CLUSTER` | `INFO` | -- | `CmdClusterInfo` | 0 |
| `TSO` | -- | -- | `CmdTSO` | 0 |
| `GC` | `SAFEPOINT` | -- | `CmdGCSafepoint` | 0 |
| `STATUS` | -- | -- | `CmdStatus` | 0 or 1 string |
| `HELP` | -- | -- | `CmdHelp` | 0 |
| `EXIT` / `QUIT` | -- | -- | `CmdExit` | 0 |

#### BEGIN Option Parsing

Tokens after `BEGIN` are scanned for option keywords (case-insensitive):
- `PESSIMISTIC` -> append `client.WithPessimistic()`
- `ASYNC_COMMIT` -> append `client.WithAsyncCommit()`
- `ONE_PC` -> append `client.With1PC()`
- `LOCK_TTL` -> consume next token as uint64 ms, append `client.WithLockTTL(n)`

Unknown tokens after BEGIN produce: `ERROR: unknown BEGIN option: <token>`.

#### Validation Rules
- `PUT` with 4+ tokens: check if 3rd token (case-insensitive) is `TTL`; if so, parse 4th as uint64 seconds.
- `BPUT` requires even number of key-value tokens after the command name.
- `CAS` requires 3 value tokens; optional 4th must be `NOT_EXIST`.
- `CHECKSUM` requires 0 or 2 key tokens (reject 1).
- Empty key validation belongs in the executor, not the parser (since `""` is valid for SCAN/DELETE RANGE range boundaries).

### Files to Create/Modify

**`cmd/gookv-cli/parser_test.go`** -- Add `TestParseCommand` with 30 cases.

#### Test Cases (30 command patterns)

| # | Tokens | inTxn | Expected Type | Key Args | Notes |
|---|--------|-------|---------------|----------|-------|
| 1 | `GET mykey` | false | `CmdGet` | `[mykey]` | Raw GET |
| 2 | `GET mykey` | true | `CmdTxnGet` | `[mykey]` | Txn GET |
| 3 | `PUT k1 v1` | false | `CmdPut` | `[k1, v1]` | Raw PUT |
| 4 | `PUT k1 v1 TTL 60` | false | `CmdPutTTL` | `[k1, v1]` IntArg=60 | PUT with TTL |
| 5 | `DELETE k1` | false | `CmdDelete` | `[k1]` | Raw DELETE |
| 6 | `DELETE k1` | true | `CmdTxnDelete` | `[k1]` | Txn DELETE |
| 7 | `DELETE RANGE a z` | false | `CmdDeleteRange` | `[a, z]` | Delete range |
| 8 | `SET k1 v1` | true | `CmdTxnSet` | `[k1, v1]` | Txn SET |
| 9 | `SET k1 v1` | false | error | | SET outside txn |
| 10 | `TTL mykey` | false | `CmdTTL` | `[mykey]` | TTL query |
| 11 | `SCAN a z` | false | `CmdScan` | `[a, z]` IntArg=0 | SCAN, default limit |
| 12 | `SCAN a z LIMIT 50` | false | `CmdScan` | `[a, z]` IntArg=50 | SCAN with LIMIT |
| 13 | `BGET k1 k2 k3` | false | `CmdBatchGet` | `[k1, k2, k3]` | Batch GET |
| 14 | `BGET k1 k2` | true | `CmdTxnBatchGet` | `[k1, k2]` | Txn Batch GET |
| 15 | `BPUT k1 v1 k2 v2` | false | `CmdBatchPut` | `[k1, v1, k2, v2]` | Batch PUT |
| 16 | `BPUT k1 v1 k2` | false | error | | Odd args |
| 17 | `BDELETE k1 k2` | false | `CmdBatchDelete` | `[k1, k2]` | Batch DELETE |
| 18 | `CAS k1 v2 v1` | false | `CmdCAS` | `[k1, v2, v1]` | CAS normal |
| 19 | `CAS k1 v2 _ NOT_EXIST` | false | `CmdCAS` | `[k1, v2, _]` flag=1 | CAS NOT_EXIST |
| 20 | `CHECKSUM` | false | `CmdChecksum` | `[]` | Full keyspace |
| 21 | `CHECKSUM a z` | false | `CmdChecksum` | `[a, z]` | Range checksum |
| 22 | `CHECKSUM a` | false | error | | Invalid: 1 arg |
| 23 | `BEGIN` | false | `CmdBegin` | opts=[] | Optimistic default |
| 24 | `BEGIN PESSIMISTIC` | false | `CmdBegin` | opts=[Pessimistic] | Pessimistic |
| 25 | `BEGIN ASYNC_COMMIT ONE_PC LOCK_TTL 5000` | false | `CmdBegin` | opts=[AsyncCommit, 1PC, LockTTL(5000)] | Combined |
| 26 | `COMMIT` | true | `CmdCommit` | | |
| 27 | `ROLLBACK` | true | `CmdRollback` | | |
| 28 | `STORE LIST` | false | `CmdStoreList` | | |
| 29 | `STORE STATUS 1` | false | `CmdStoreStatus` | IntArg=1 | |
| 30 | `REGION mykey` | false | `CmdRegion` | `[mykey]` | |
| 31 | `REGION LIST` | false | `CmdRegionList` | IntArg=0 | Default limit |
| 32 | `REGION LIST LIMIT 10` | false | `CmdRegionList` | IntArg=10 | |
| 33 | `REGION ID 42` | false | `CmdRegionByID` | IntArg=42 | |
| 34 | `CLUSTER INFO` | false | `CmdClusterInfo` | | |
| 35 | `TSO` | false | `CmdTSO` | | |
| 36 | `GC SAFEPOINT` | false | `CmdGCSafepoint` | | |
| 37 | `STATUS` | false | `CmdStatus` | StrArg="" | No address |
| 38 | `STATUS 127.0.0.1:20180` | false | `CmdStatus` | StrArg="127.0.0.1:20180" | With address |
| 39 | `HELP` | false | `CmdHelp` | | |
| 40 | `EXIT` | false | `CmdExit` | | |
| 41 | `QUIT` | false | `CmdExit` | | |
| 42 | `get MYKEY` | false | `CmdGet` | `[MYKEY]` | Case insensitive cmd |
| 43 | (empty tokens) | false | error | | Empty statement |

#### Test Cases for `ParseMetaCommand`

| # | Input | Expected Type | Notes |
|---|-------|---------------|-------|
| 1 | `\help` | `CmdHelp` | |
| 2 | `\h` | `CmdHelp` | Short form |
| 3 | `\?` | `CmdHelp` | Alt form |
| 4 | `\quit` | `CmdExit` | |
| 5 | `\q` | `CmdExit` | Short form |
| 6 | `\timing on` | `CmdMetaTiming` | StrArg="on" |
| 7 | `\timing off` | `CmdMetaTiming` | StrArg="off" |
| 8 | `\format hex` | `CmdMetaFormat` | StrArg="hex" |
| 9 | `\format table` | `CmdMetaFormat` | StrArg="table" |
| 10 | `\format plain` | `CmdMetaFormat` | StrArg="plain" |
| 11 | `\pagesize 50` | `CmdMetaPagesize` | IntArg=50 |
| 12 | `\x` | `CmdMetaHex` | Toggle hex |
| 13 | `\t` | `CmdMetaTiming` | Short timing toggle |
| 14 | `\unknown` | error | Unknown meta command |

### Acceptance Criteria

1. `go test ./cmd/gookv-cli/ -run TestParseCommand` passes all 43 cases.
2. `go test ./cmd/gookv-cli/ -run TestParseMetaCommand` passes all 14 cases.
3. Context-sensitive dispatch: `GET` inside txn produces `CmdTxnGet`, outside produces `CmdGet`.
4. `SET` outside a transaction produces a descriptive error.
5. Multi-word commands (`DELETE RANGE`, `STORE LIST`, `REGION ID`, `GC SAFEPOINT`, `CLUSTER INFO`) parse correctly.
6. Case insensitivity: `get`, `GET`, `Get` all produce the same CommandType.

---

## Step 4: Implement Output Formatter

### Goal
Render `Result` structs to stdout in table, plain, or hex mode, with optional timing display.

### Files to Create

**`cmd/gookv-cli/formatter.go`** (replace stub)

#### Types

```go
type DisplayMode int

const (
    DisplayAuto   DisplayMode = iota // string if printable, hex otherwise
    DisplayHex                       // always hex
    DisplayString                    // always string (non-printable as \xNN)
)

type OutputFormat int

const (
    FormatTable OutputFormat = iota // ASCII box tables (default)
    FormatPlain                     // tab-separated, one pair per line
    FormatHex                       // like plain but always hex-encoded
)

type ResultType int

const (
    ResultOK       ResultType = iota // simple "OK" (PUT, DELETE, COMMIT, etc.)
    ResultValue                      // single value (GET)
    ResultNotFound                   // key not found
    ResultNil                        // txn not found (displays "(nil)")
    ResultRows                       // multi-row key-value table (SCAN, BGET)
    ResultTable                      // arbitrary column table (STORE LIST, CLUSTER INFO)
    ResultScalar                     // single scalar line (TTL, GC SAFEPOINT)
    ResultCAS                        // CAS result (swapped, previous value)
    ResultChecksum                   // checksum result
    ResultBegin                      // BEGIN output (startTS, mode description)
    ResultCommit                     // COMMIT output (commitTS, key count)
)

type Result struct {
    Type       ResultType
    Value      []byte
    Rows       []KvPairResult     // for ResultRows
    Columns    []string           // for ResultTable
    TableRows  [][]string         // for ResultTable
    Scalar     string             // for ResultScalar
    Message    string             // for ResultOK with extra info, ResultBegin, ResultCommit
    Swapped    bool               // for ResultCAS
    PrevVal    []byte             // for ResultCAS
    PrevNotFound bool             // for ResultCAS (prev key did not exist)
    Checksum   uint64             // for ResultChecksum
    TotalKvs   uint64             // for ResultChecksum
    TotalBytes uint64             // for ResultChecksum
    NotFoundCount int             // for ResultRows (BGET: count of keys not found)
}

type KvPairResult struct {
    Key   []byte
    Value []byte
}

type Formatter struct {
    out         io.Writer
    errOut      io.Writer       // stderr for errors
    displayMode DisplayMode
    outputFmt   OutputFormat
    showTiming  bool            // default: true
    pageSize    int             // default SCAN limit: 100
}

func NewFormatter(out io.Writer) *Formatter

func (f *Formatter) Format(result *Result, elapsed time.Duration)
func (f *Formatter) FormatError(err error)
func (f *Formatter) SetDisplayMode(mode DisplayMode)
func (f *Formatter) SetOutputFormat(fmt OutputFormat)
func (f *Formatter) SetTiming(on bool)
func (f *Formatter) SetPageSize(n int)
func (f *Formatter) PageSize() int
func (f *Formatter) formatBytes(data []byte) string
```

#### Display Logic
- `formatBytes`: In `DisplayAuto` mode, returns the string if all bytes are printable ASCII (0x20--0x7E); otherwise returns `\xNN` hex encoding. In `DisplayHex` mode, always hex. In `DisplayString` mode, always as string with `\xNN` for non-printable bytes.
- `ResultValue`: In `FormatTable`, render a single-cell table with header "value". In `FormatPlain`/`FormatHex`, just print the value.
- `ResultRows`: In `FormatTable`, use tablewriter with headers ["Key", "Value"]. In `FormatPlain`, tab-separated one pair per line. In `FormatHex`, same as plain but hex-encoded.
- `ResultNotFound`: Print `(not found)`.
- `ResultNil`: Print `(nil)`.
- Timing: Appended as `(N.Nms)` when `showTiming` is true. For `ResultRows`, format as `(N rows, N.Nms)`. For `ResultRows` with `NotFoundCount > 0`, format as `(N rows, M not found, N.Nms)`.

### External Dependency

Add `github.com/olekukonko/tablewriter v0.0.5` to `go.mod`:

```bash
go get github.com/olekukonko/tablewriter@v0.0.5
```

### Files to Create

**`cmd/gookv-cli/formatter_test.go`** (new)

#### Test Cases

**Table mode (5 cases):**
| # | ResultType | Input | Expected Contains |
|---|------------|-------|-------------------|
| 1 | ResultOK | -- | `"OK"` |
| 2 | ResultValue | `[]byte("hello")` | table with "hello" in value column |
| 3 | ResultNotFound | -- | `"(not found)"` |
| 4 | ResultRows (3 pairs) | -- | 3-row table with Key/Value headers |
| 5 | ResultTable (store list) | 3 columns, 2 rows | headers + 2 data rows |

**Plain mode (5 cases):**
| # | ResultType | Input | Expected |
|---|------------|-------|----------|
| 1 | ResultOK | -- | `"OK\n"` |
| 2 | ResultValue | `"hello"` | `"hello\n"` |
| 3 | ResultNotFound | -- | `"(not found)\n"` |
| 4 | ResultRows (2 pairs) | -- | `"k1\tv1\nk2\tv2\n"` |
| 5 | ResultNil | -- | `"(nil)\n"` |

**Hex mode (5 cases):**
| # | ResultType | Input | Expected |
|---|------------|-------|----------|
| 1 | ResultValue printable | `"hello"` | `"68656c6c6f\n"` |
| 2 | ResultValue binary | `\x00\x01` | `"0001\n"` |
| 3 | ResultRows | -- | hex-encoded keys and values |
| 4 | formatBytes auto, printable | `"abc"` | `"abc"` |
| 5 | formatBytes auto, binary | `\x00\xff` | `"\x00\xff"` |

**Timing (2 cases):**
| # | Timing On | Expected |
|---|-----------|----------|
| 1 | true | output ends with `(N.Nms)` |
| 2 | false | no timing in output |

**Special result types (5 cases):**
| # | ResultType | Expected Contains |
|---|------------|-------------------|
| 1 | ResultCAS swapped | `"OK (swapped)"`, previous value |
| 2 | ResultCAS not swapped | `"FAILED (not swapped)"`, previous value |
| 3 | ResultChecksum | checksum hex, total keys, total bytes |
| 4 | ResultBegin | `"Transaction started"` with mode and startTS |
| 5 | ResultCommit | `"OK"` with commitTS and key count |

### Acceptance Criteria

1. `go test ./cmd/gookv-cli/ -run TestFormatter` passes all cases.
2. Table output matches the ASCII box format from the spec (using tablewriter).
3. `formatBytes` correctly detects printable vs binary content.
4. Timing display respects the on/off toggle.
5. Error output goes to stderr, not stdout.

---

## Step 5: Implement Raw KV Executor

### Goal
Implement handlers for all 12 Raw KV commands, dispatching to `RawKVClient` methods and returning `Result` structs.

### Files to Modify

**`cmd/gookv-cli/executor.go`** (replace stub)

#### Types

```go
type Executor struct {
    client    *client.Client
    rawkv     *client.RawKVClient
    txnkv     *client.TxnKVClient
    pdClient  pdclient.Client

    activeTxn *client.TxnHandle // nil when not in a transaction
}

func NewExecutor(c *client.Client, pd pdclient.Client) *Executor

// Exec dispatches a command and returns the result.
func (e *Executor) Exec(ctx context.Context, cmd Command) (*Result, error)

// InTransaction returns true if a transaction is active.
func (e *Executor) InTransaction() bool
```

#### Raw KV Handlers

| CommandType | Handler Logic |
|---|---|
| `CmdGet` | `rawkv.Get(ctx, key)` -> check `notFound` bool -> `ResultValue` or `ResultNotFound` |
| `CmdPut` | `rawkv.Put(ctx, key, val)` -> `ResultOK` |
| `CmdPutTTL` | `rawkv.PutWithTTL(ctx, key, val, uint64(intArg))` -> `ResultOK` |
| `CmdDelete` | `rawkv.Delete(ctx, key)` -> `ResultOK` |
| `CmdTTL` | `rawkv.GetKeyTTL(ctx, key)` -> `ResultScalar` with `"TTL: Ns"` or `"TTL: 0 (no expiration)"` |
| `CmdScan` | `rawkv.Scan(ctx, start, end, limit)` -> `ResultRows` (limit defaults to `formatter.PageSize()` if IntArg==0) |
| `CmdBatchGet` | `rawkv.BatchGet(ctx, keys)` -> `ResultRows` with `NotFoundCount = len(keys) - len(results)` |
| `CmdBatchPut` | `rawkv.BatchPut(ctx, pairs)` -> `ResultOK` with message `"OK (N pairs)"` |
| `CmdBatchDelete` | `rawkv.BatchDelete(ctx, keys)` -> `ResultOK` with message `"OK (N keys)"` |
| `CmdDeleteRange` | `rawkv.DeleteRange(ctx, start, end)` -> `ResultOK` |
| `CmdCAS` | `rawkv.CompareAndSwap(ctx, key, newVal, oldVal, notExist)` -> `ResultCAS` |
| `CmdChecksum` | `rawkv.Checksum(ctx, start, end)` -> `ResultChecksum` |

#### Validation in Executor
- `CmdGet`, `CmdPut`, `CmdDelete`, `CmdTTL`: reject empty key with `"key must not be empty"`.
- `CmdDeleteRange` with both keys `""`: in interactive mode, the REPL layer should prompt for confirmation (handled in Step 8).

### Testing Strategy

Since `RawKVClient` is a concrete struct (not an interface), integration tests require either:
1. **Interface extraction**: Define a `RawKVExecutable` interface in the CLI package matching the methods used, and have the executor accept the interface. This enables mock-based unit testing.
2. **E2E tests only**: Skip unit tests for the executor and rely on E2E tests (Step 10).

**Recommended approach**: Option 1 -- define narrow interfaces in the executor file:

```go
// rawKVAPI is the subset of client.RawKVClient methods used by the executor.
type rawKVAPI interface {
    Get(ctx context.Context, key []byte) ([]byte, bool, error)
    Put(ctx context.Context, key, value []byte) error
    PutWithTTL(ctx context.Context, key, value []byte, ttl uint64) error
    Delete(ctx context.Context, key []byte) error
    GetKeyTTL(ctx context.Context, key []byte) (uint64, error)
    BatchGet(ctx context.Context, keys [][]byte) ([]client.KvPair, error)
    BatchPut(ctx context.Context, pairs []client.KvPair) error
    BatchDelete(ctx context.Context, keys [][]byte) error
    Scan(ctx context.Context, startKey, endKey []byte, limit int) ([]client.KvPair, error)
    DeleteRange(ctx context.Context, startKey, endKey []byte) error
    CompareAndSwap(ctx context.Context, key, value, prevValue []byte, prevNotExist bool) (bool, []byte, error)
    Checksum(ctx context.Context, startKey, endKey []byte) (uint64, uint64, uint64, error)
}
```

`client.RawKVClient` already satisfies this interface. Tests provide a mock implementation.

### Files to Create

**`cmd/gookv-cli/executor_test.go`** (new) -- mock `rawKVAPI`, test each of the 12 raw KV handlers.

### Acceptance Criteria

1. `go test ./cmd/gookv-cli/ -run TestExecRawGet` -- covers found/not-found/error.
2. `go test ./cmd/gookv-cli/ -run TestExecRawPut` -- covers success/error.
3. `go test ./cmd/gookv-cli/ -run TestExecRawPutTTL` -- covers success.
4. `go test ./cmd/gookv-cli/ -run TestExecRawDelete` -- covers success.
5. `go test ./cmd/gookv-cli/ -run TestExecRawTTL` -- covers TTL value and no-expiration case.
6. `go test ./cmd/gookv-cli/ -run TestExecRawScan` -- covers results, empty, default limit.
7. `go test ./cmd/gookv-cli/ -run TestExecRawBatchGet` -- covers partial results, not-found count.
8. `go test ./cmd/gookv-cli/ -run TestExecRawBatchPut` -- covers success, pair count message.
9. `go test ./cmd/gookv-cli/ -run TestExecRawBatchDelete` -- covers success.
10. `go test ./cmd/gookv-cli/ -run TestExecRawDeleteRange` -- covers success.
11. `go test ./cmd/gookv-cli/ -run TestExecRawCAS` -- covers swapped/not-swapped/NOT_EXIST.
12. `go test ./cmd/gookv-cli/ -run TestExecRawChecksum` -- covers checksum output.
13. Empty key rejection for single-key commands.

---

## Step 6: Implement Transaction Executor

### Goal
Implement handlers for BEGIN, SET, GET, DELETE, BGET (in txn), COMMIT, ROLLBACK with transaction state tracking.

### Files to Modify

**`cmd/gookv-cli/executor.go`** -- Add txn handler methods.

#### Handler Logic

| CommandType | Handler Logic |
|---|---|
| `CmdBegin` | Reject if `activeTxn != nil`. Call `txnkv.Begin(ctx, opts...)`. Set `activeTxn`. Return `ResultBegin` with startTS and mode description. |
| `CmdTxnSet` | Reject if `activeTxn == nil`. Call `activeTxn.Set(ctx, key, val)`. Return `ResultOK`. |
| `CmdTxnGet` | Reject if `activeTxn == nil`. Call `activeTxn.Get(ctx, key)`. If `val == nil && err == nil`, return `ResultNil`. Otherwise `ResultValue`. |
| `CmdTxnDelete` | Reject if `activeTxn == nil`. Call `activeTxn.Delete(ctx, key)`. Return `ResultOK`. |
| `CmdTxnBatchGet` | Reject if `activeTxn == nil`. Call `activeTxn.BatchGet(ctx, keys)`. Return `ResultRows`. |
| `CmdCommit` | Reject if `activeTxn == nil`. Call `activeTxn.Commit(ctx)`. On success or "already committed": set `activeTxn = nil`, return `ResultCommit`. On other failure: keep `activeTxn` alive (user should ROLLBACK), return error. |
| `CmdRollback` | Reject if `activeTxn == nil`. Call `activeTxn.Rollback(ctx)`. Set `activeTxn = nil`. Return `ResultOK` with `"OK (rolled back)"`. |

#### Not-Found Detection for TxnHandle.Get

Per review finding [Impl-3], `TxnHandle.Get(ctx, key)` returns `([]byte, error)` (no bool). Not-found is detected by `val == nil && err == nil`. This differs from `RawKVClient.Get` which has an explicit `notFound` bool.

#### Commit Failure Handling

Per `02_command_reference.md` Section 3.5: on commit failure (except "already committed"), the transaction remains active so the user can ROLLBACK to clean up locks. The executor checks whether the error matches `client.ErrTxnCommitted` before deciding whether to nil out `activeTxn`.

### Interface for Testing

```go
type txnKVAPI interface {
    Begin(ctx context.Context, opts ...client.TxnOption) (*client.TxnHandle, error)
}
```

`TxnHandle` methods are called directly on the handle (no additional interface needed for unit testing -- mock the `txnKVAPI.Begin` to return a handle backed by a mock, or test at the E2E level).

Given that `TxnHandle` is a concrete struct with unexported fields, the simplest approach for unit testing is:
1. Mock `txnKVAPI` to track Begin calls.
2. For the actual txn operations (Set/Get/Delete/Commit/Rollback), test via E2E with a real cluster.
3. Alternatively, define a `txnHandleAPI` interface.

### Files to Modify

**`cmd/gookv-cli/executor_test.go`** -- Add transaction lifecycle tests.

### Test Cases

| # | Test Name | Scenario |
|---|-----------|----------|
| 1 | TestTxnBeginOptimistic | BEGIN with no options, verify ResultBegin |
| 2 | TestTxnBeginPessimistic | BEGIN PESSIMISTIC, verify mode in output |
| 3 | TestTxnBeginAlreadyActive | BEGIN when txn active -> error |
| 4 | TestTxnSetOutsideTxn | SET without BEGIN -> error |
| 5 | TestTxnGetNotFound | GET inside txn returns nil -> ResultNil |
| 6 | TestTxnGetValue | GET inside txn returns value -> ResultValue |
| 7 | TestTxnCommitSuccess | COMMIT -> activeTxn becomes nil |
| 8 | TestTxnCommitFailure | COMMIT fails -> activeTxn stays active |
| 9 | TestTxnRollback | ROLLBACK -> activeTxn becomes nil |
| 10 | TestTxnRollbackWithoutTxn | ROLLBACK without BEGIN -> error |
| 11 | TestTxnLifecycle | BEGIN -> SET -> GET -> COMMIT full cycle |

### Acceptance Criteria

1. All 11 transaction tests pass.
2. Transaction state transitions are correct: `activeTxn` is non-nil between BEGIN and COMMIT/ROLLBACK.
3. Commit failure preserves the transaction handle.
4. `TxnHandle.Get` not-found detection uses `val == nil && err == nil`.
5. `InTransaction()` returns correct state at each lifecycle point.

---

## Step 7: Implement Admin Executor

### Goal
Implement handlers for all administrative commands using `pdclient.Client`.

### Files to Modify

**`cmd/gookv-cli/executor.go`** -- Add admin handler methods.

#### Handler Logic

| CommandType | Handler Logic |
|---|---|
| `CmdStoreList` | `pdClient.GetAllStores(ctx)` -> `ResultTable` with columns [StoreID, Address, State]. Summary: `"(N stores)"`. |
| `CmdStoreStatus` | `pdClient.GetStore(ctx, storeID)` -> `ResultScalar` with formatted store details. |
| `CmdRegion` | `pdClient.GetRegion(ctx, key)` -> `ResultScalar` with region ID, start/end keys (hex), epoch, peers, leader. |
| `CmdRegionList` | Walk keyspace: start from `""`, call `pdClient.GetRegion(ctx, key)`, advance by `EndKey`. Stop when `EndKey` is empty or limit reached. -> `ResultTable` with columns [RegionID, StartKey, EndKey, Peers, Leader]. |
| `CmdRegionByID` | `pdClient.GetRegionByID(ctx, regionID)` -> `ResultScalar` with region details. |
| `CmdClusterInfo` | `pdClient.GetClusterID(ctx)` + `pdClient.GetAllStores(ctx)` + region walk count. -> Composite `ResultScalar` with cluster ID, store table, region count. |
| `CmdTSO` | `pdClient.GetTS(ctx)` -> `ResultScalar`. Display raw uint64, decomposed physical (with UTC time), and logical. Use `pdclient.TimeStampFromUint64` or `ts.Physical`/`ts.Logical` directly since `GetTS` returns a `TimeStamp` struct. |
| `CmdGCSafepoint` | `pdClient.GetGCSafePoint(ctx)` -> `ResultScalar`. Display raw uint64, decomposed physical/logical. If 0: `"0 (not set)"`. |
| `CmdStatus` | HTTP GET to `http://<addr>/status`. Parse JSON response. -> `ResultScalar` with formatted JSON. If no addr provided, use a reasonable default or error. |

#### TSO Decomposition

`pdclient.Client.GetTS(ctx)` returns `pdclient.TimeStamp{Physical: int64, Logical: int64}`. The raw uint64 form is `Physical<<18 | Logical`. Format the timestamp as:

```
Timestamp:  <raw_uint64>
  physical: <physical_ms> (<UTC_time>)
  logical:  <logical>
```

The UTC time is derived from `time.UnixMilli(ts.Physical).UTC().Format(time.RFC3339)`.

#### Region Info Formatting

For REGION output, keys are displayed as hex with an ASCII hint:

```
Region ID:  2
  StartKey:  6163636f756e743a (account:)
  EndKey:    6163636f756e743a6d (account:m)
  Epoch:     conf_ver:N version:N
  Peers:     [store:1, store:2, store:3]
  Leader:    store:2
```

The `metapb.Store` State field is `metapb.StoreState` enum. Map to human-readable: `0 -> "Up"`, `1 -> "Offline"`, `2 -> "Tombstone"`.

### Testing with MockClient

The existing `pdclient.MockClient` (in `pkg/pdclient/mock.go`) implements the full `pdclient.Client` interface, so admin command tests can use it directly without new mocks.

### Files to Modify

**`cmd/gookv-cli/executor_test.go`** -- Add admin command tests.

### Test Cases

| # | Test Name | Scenario |
|---|-----------|----------|
| 1 | TestAdminStoreList | 3 stores, verify table columns and row count |
| 2 | TestAdminStoreListEmpty | No stores registered |
| 3 | TestAdminStoreStatus | Store found, verify fields |
| 4 | TestAdminStoreStatusNotFound | Store not found -> error |
| 5 | TestAdminRegion | Key maps to region, verify output fields |
| 6 | TestAdminRegionList | Walk keyspace, verify all regions listed |
| 7 | TestAdminRegionListLimit | LIMIT 2 stops at 2 |
| 8 | TestAdminRegionByID | Region found by ID |
| 9 | TestAdminClusterInfo | Cluster ID + store count + region count |
| 10 | TestAdminTSO | Verify timestamp decomposition |
| 11 | TestAdminGCSafepoint | Non-zero safepoint |
| 12 | TestAdminGCSafepointZero | Zero safepoint -> "not set" |

### Acceptance Criteria

1. All 12 admin tests pass using `pdclient.MockClient`.
2. Store state enum maps correctly to "Up"/"Offline"/"Tombstone".
3. TSO decomposition matches `pdclient.TimeStampFromUint64` logic.
4. Region list walk terminates correctly on empty EndKey.
5. Hex key display with ASCII hint works for printable keys.

---

## Step 8: Implement REPL Loop

### Goal
Wire the REPL loop with readline for line editing, history, prompt management, Ctrl-C/Ctrl-D handling, stdin pipe detection, and per-command context cancellation.

### Files to Modify

**`cmd/gookv-cli/repl.go`** (replace stub)

#### External Dependency

Add `github.com/chzyer/readline v1.5.1` to `go.mod`:

```bash
go get github.com/chzyer/readline@v1.5.1
```

#### REPL State Machine

```go
type replState int

const (
    stateIdle          replState = iota // ready for new statement
    stateCollecting                     // accumulating multi-line input
    stateInTxn                          // inside a transaction
    stateTxnCollecting                  // multi-line inside a transaction
)
```

#### Prompt Table

| State | Prompt |
|---|---|
| `stateIdle` | `"gookv> "` |
| `stateCollecting` | `"     > "` |
| `stateInTxn` | `"gookv(txn)> "` |
| `stateTxnCollecting` | `"         > "` |

#### Key Behaviors

1. **Line read**: `readline.Instance.Readline()` returns the line, or `readline.ErrInterrupt` (Ctrl-C), or `io.EOF` (Ctrl-D).

2. **Meta command detection**: Before appending to the buffer, check `IsMetaCommand(line)`. If true, execute immediately (no semicolon needed) via `ParseMetaCommand` and return to prompt.

3. **Buffer accumulation**: Append line to buffer. Call `SplitStatements(buffer)`. For each complete statement, tokenize, parse (passing `exec.InTransaction()`), execute, and format. If there's a remainder, keep it in the buffer and switch to collecting state.

4. **Ctrl-C handling**: Discard the buffer and return to the non-collecting state (Idle or InTxn). Does NOT rollback a transaction or exit the CLI.

5. **Ctrl-D handling**: On empty buffer, exit the CLI. If a transaction is active, print warning and auto-rollback.

6. **Per-command context** (resolves review [E-5]): Create a child context for each command execution. The REPL can cancel this child context on Ctrl-C during execution without killing the entire CLI.

```go
// Per-command context pattern
cmdCtx, cmdCancel := context.WithCancel(ctx)
defer cmdCancel()
// If readline gets Ctrl-C while Exec is running, cancel cmdCtx
result, err := exec.Exec(cmdCtx, cmd)
```

7. **Stdin pipe detection**: At startup, check `os.Stdin.Stat()` for `ModeCharDevice`. If not a TTY, skip readline initialization and read line-by-line from `bufio.Scanner`. No prompt displayed. Exit with code 1 on first error.

8. **Connected message**: On startup, print `"Connected to gookv cluster (PD: <endpoints>)"`.

#### `runBatch` Function

```go
func runBatch(ctx context.Context, exec *Executor, fmtr *Formatter, input string) int {
    stmts, _, _ := SplitStatements(input)
    for _, stmt := range stmts {
        tokens, err := Tokenize(strings.TrimSpace(stmt))
        if err != nil { ... return 1 }
        if len(tokens) == 0 { continue }
        cmd, err := ParseCommand(tokens, exec.InTransaction())
        if err != nil { ... return 1 }
        start := time.Now()
        result, err := exec.Exec(ctx, cmd)
        elapsed := time.Since(start)
        if err != nil { fmtr.FormatError(err); return 1 }
        fmtr.Format(result, elapsed)
    }
    return 0
}
```

### Files to Modify

**`cmd/gookv-cli/main.go`** -- Pass PD endpoints to REPL for the connected message. Detect stdin pipe mode.

### Acceptance Criteria

1. `gookv-cli --pd <addr>` enters REPL mode with `gookv> ` prompt (manual test).
2. Multi-line input works: type `PUT k1` [Enter], prompt changes to `     > `, type `v1;` [Enter], command executes.
3. Ctrl-C discards partial input and returns to prompt.
4. Ctrl-D on empty line exits with `"Goodbye."`.
5. Ctrl-D with active transaction prints warning, auto-rollback, then exits.
6. Prompt changes to `gookv(txn)> ` after BEGIN, reverts after COMMIT/ROLLBACK.
7. `echo "PUT k1 v1; GET k1;" | gookv-cli --pd <addr>` works (stdin pipe mode, no prompt).
8. `-c "PUT k1 v1; GET k1;"` executes both statements and exits.
9. History is saved to `~/.gookv_history`.

---

## Step 9: Implement Meta Commands

### Goal
Wire all meta commands (backslash and keyword forms) to their handlers.

### Files to Modify

**`cmd/gookv-cli/executor.go`** -- Add meta command handlers.

**`cmd/gookv-cli/repl.go`** -- Handle meta commands in the REPL loop before the parse/execute pipeline.

#### Meta Command Handlers

| CommandType | Handler |
|---|---|
| `CmdHelp` | Print the help text (same as `02_command_reference.md` Section 5.1 HELP output). |
| `CmdExit` | Set an exit flag. If `activeTxn != nil`, prompt for confirmation (interactive mode) or auto-rollback (batch mode). |
| `CmdMetaTiming` | If StrArg is "on" -> `fmtr.SetTiming(true)`. If "off" -> `fmtr.SetTiming(false)`. If empty (from `\t`) -> toggle. Print confirmation: `"Timing: ON"` or `"Timing: OFF"`. |
| `CmdMetaFormat` | Set output format from StrArg: "table" -> `FormatTable`, "plain" -> `FormatPlain`, "hex" -> `FormatHex`. Print confirmation. |
| `CmdMetaPagesize` | Set `fmtr.SetPageSize(intArg)`. Print `"Page size: N"`. |
| `CmdMetaHex` | Cycle display mode: `DisplayAuto -> DisplayHex -> DisplayString -> DisplayAuto`. Print current mode. |

#### Help Text

The help text matches the canonical command set from the addenda:

```
Raw KV Commands:
  GET <key>                          Get a value
  PUT <key> <value> [TTL <sec>]      Put a value (optional TTL)
  DELETE <key>                       Delete a key
  SCAN <start> <end> [LIMIT <n>]     Scan a key range
  BGET <k1> <k2> ...                 Batch get
  BPUT <k1> <v1> <k2> <v2> ...       Batch put
  BDELETE <k1> <k2> ...              Batch delete
  DELETE RANGE <start> <end>          Delete a key range
  TTL <key>                           Get remaining TTL
  CAS <key> <new> <old> [NOT_EXIST]   Compare and swap
  CHECKSUM [<start> <end>]            Compute range checksum

Transaction Commands:
  BEGIN [PESSIMISTIC] [ASYNC_COMMIT] [ONE_PC] [LOCK_TTL <ms>]
  SET <key> <value>                   Set within transaction
  GET <key>                           Get within transaction
  DELETE <key>                        Delete within transaction
  BGET <k1> <k2> ...                 Batch get within transaction
  COMMIT                              Commit transaction
  ROLLBACK                            Rollback transaction

Admin Commands:
  STORE LIST                          List all stores
  STORE STATUS <id>                   Show store details
  REGION <key>                        Find region for key
  REGION LIST [LIMIT <n>]             List all regions
  REGION ID <id>                      Find region by ID
  CLUSTER INFO                        Show cluster overview
  TSO                                 Allocate a timestamp
  GC SAFEPOINT                        Show GC safe point
  STATUS [<addr>]                     Query server status

Meta Commands:
  \help, \h, \?                       Show this help
  \quit, \q                           Exit the REPL
  \timing on|off, \t                  Toggle timing display
  \format table|plain|hex             Set output format
  \pagesize <n>                       Set default SCAN limit
  \x                                  Cycle display mode (auto/hex/string)
  HELP;  EXIT;  QUIT;                 Keyword aliases (require ;)

Keys: Use 0x... for hex, "..." for quoted strings, or bare words.
```

### Keyword Alias Handling

`HELP;`, `EXIT;`, and `QUIT;` are regular semicolon-terminated statements. The parser (Step 3) already maps these to `CmdHelp` and `CmdExit`. The executor dispatches them the same as their backslash counterparts.

### Test Cases

| # | Test Name | Scenario |
|---|-----------|----------|
| 1 | TestMetaHelp | `\help` and `HELP;` both produce help text |
| 2 | TestMetaQuit | `\quit` sets exit flag |
| 3 | TestMetaTimingOn | `\timing on` enables timing |
| 4 | TestMetaTimingOff | `\timing off` disables timing |
| 5 | TestMetaTimingToggle | `\t` toggles timing |
| 6 | TestMetaFormatTable | `\format table` sets FormatTable |
| 7 | TestMetaFormatPlain | `\format plain` sets FormatPlain |
| 8 | TestMetaFormatHex | `\format hex` sets FormatHex |
| 9 | TestMetaPagesize | `\pagesize 50` sets page size to 50 |
| 10 | TestMetaHexCycle | `\x` cycles through Auto -> Hex -> String -> Auto |
| 11 | TestExitWithActiveTxn | EXIT with active txn triggers rollback |

### Acceptance Criteria

1. All 11 meta command tests pass.
2. `\help` output matches the canonical help text.
3. `\timing`, `\format`, `\pagesize` correctly modify formatter state.
4. `\x` cycles through all three display modes.
5. EXIT/QUIT with active transaction auto-rollbacks.

---

## Step 10: Makefile + Build Integration + go.mod

### Goal
Finalize build integration, add new dependencies to go.mod, verify the full binary builds and links.

### Files to Modify

**`Makefile`** -- Ensure `gookv-cli` target is in `build:` (done in Step 1), add optional `clean:` target.

```makefile
build: gookv-server gookv-ctl gookv-pd gookv-cli

gookv-cli: $(GO_SRC) go.mod go.sum
	go build -o gookv-cli ./cmd/gookv-cli

clean:
	rm -f gookv-server gookv-ctl gookv-pd gookv-cli
```

**`go.mod`** -- Add new dependencies:

```
require (
    github.com/chzyer/readline v1.5.1
    github.com/olekukonko/tablewriter v0.0.5
)
```

Run `go mod tidy` to update `go.sum`.

**`.phony` list** -- Add `clean` to the `.PHONY` line.

### Verification Commands

```bash
# Full build
make build

# Verify binary exists and runs
./gookv-cli --version

# Unit tests
go test ./cmd/gookv-cli/... -v -count=1

# Vet
go vet ./cmd/gookv-cli/...

# Existing tests still pass
make test
```

### Acceptance Criteria

1. `make build` produces all four binaries: `gookv-server`, `gookv-ctl`, `gookv-pd`, `gookv-cli`.
2. `make clean` removes all binaries.
3. `./gookv-cli --version` prints version string.
4. `go test ./cmd/gookv-cli/... -v` passes all unit tests from Steps 2--9.
5. `go vet ./cmd/gookv-cli/...` reports no issues.
6. `make test` (existing unit tests) still passes -- the new `PD()` accessor on `pkg/client.Client` does not break anything.
7. `go mod tidy` produces no changes (dependencies are clean).

---

## Summary: File Inventory

### New Files

| File | Step | Purpose |
|---|---|---|
| `cmd/gookv-cli/main.go` | 1 | Entry point, flags, client init |
| `cmd/gookv-cli/parser.go` | 2, 3 | Tokenizer, splitter, command parser |
| `cmd/gookv-cli/parser_test.go` | 2, 3 | Parser unit tests |
| `cmd/gookv-cli/executor.go` | 5, 6, 7 | Command dispatcher |
| `cmd/gookv-cli/executor_test.go` | 5, 6, 7 | Executor unit tests |
| `cmd/gookv-cli/formatter.go` | 4 | Output rendering |
| `cmd/gookv-cli/formatter_test.go` | 4 | Formatter unit tests |
| `cmd/gookv-cli/repl.go` | 8 | REPL loop, readline, state machine |

### Modified Files

| File | Step | Change |
|---|---|---|
| `pkg/client/client.go` | 1 | Add `PD() pdclient.Client` method |
| `Makefile` | 1, 10 | Add `gookv-cli` target, `clean` target |
| `go.mod` | 10 | Add readline + tablewriter deps |
| `go.sum` | 10 | Updated by `go mod tidy` |

### Dependency on Prior Steps

```
Step 1 (scaffold) ──────────────────────────────────┐
Step 2 (tokenizer/splitter) ──┐                      │
Step 3 (command parser) ──────┼─── Step 8 (REPL) ───┤
Step 4 (formatter) ───────────┘         │            │
Step 5 (raw KV executor) ──────────────┤            │
Step 6 (txn executor) ─────────────────┤            │
Step 7 (admin executor) ───────────────┘            │
Step 9 (meta commands) ── depends on 8 ─────────────┤
Step 10 (build integration) ── depends on all ──────┘
```

Steps 2, 3, 4 can be developed in parallel. Steps 5, 6, 7 can be developed in parallel after Step 1. Step 8 requires Steps 2--4 and is needed by Step 9. Step 10 is final integration.

---

## Addendum: Review Feedback Incorporated

Resolutions for review findings from `review_notes.md` (2026-03-28).
`01_detailed_design.md` is the canonical reference for all types and signatures.
The following items in this document (`02_implementation_steps.md`) must be
updated to match.

### A1. `NewExecutor` signature: 1-arg form (R-1)

Step 1 `main.go` (line 65) and Step 5 (line 640) use a 2-arg form:

```go
// WRONG (02_implementation_steps.md):
exec := NewExecutor(c, pdClient)
func NewExecutor(c *client.Client, pd pdclient.Client) *Executor
```

The canonical form from `01_detailed_design.md` Section 3.2 is the 1-arg form.
The executor obtains the PD client internally via `c.PD()`:

```go
// CORRECT (01_detailed_design.md):
exec := NewExecutor(c)
func NewExecutor(c *client.Client) *Executor {
    e := &Executor{
        client:           c,
        rawkv:            c.RawKV(),
        txnkv:            c.TxnKV(),
        pdClient:         c.PD(),
        defaultScanLimit: 100,
        handlers:         make(map[CommandType]CommandHandler),
    }
    e.registerHandlers()
    return e
}
```

Affected locations: Step 1 `main.go` (`NewExecutor(c, pdClient)` becomes
`NewExecutor(c)`), Step 5 `NewExecutor` definition, and the separate `pdClient`
variable in `main()` is no longer needed.

### A2. `SplitStatements` return signature: 2-return form (R-2)

Step 2 (line 160) defines a 3-return form:

```go
// WRONG (02_implementation_steps.md):
func SplitStatements(input string) (stmts []string, remainder string, complete bool)
```

The canonical form from `01_detailed_design.md` Section 2.1 is the 2-return form:

```go
// CORRECT (01_detailed_design.md):
func SplitStatements(input string) (stmts []string, complete bool)
```

The `remainder` return value is unnecessary because the REPL maintains its own
buffer. When `complete` is false, the REPL keeps the entire buffer for the next
line of input. The `runBatch` function in Step 8 (line 948) which uses
`stmts, _, _ := SplitStatements(input)` should become `stmts, _ := SplitStatements(input)`.

Test cases in Step 2 that include a "Remainder" column should be adjusted: the
remainder is implicit (any unterminated trailing text is not included in `stmts`,
and `complete` is false).

### A3. `NewFormatter` signature: 2-arg form (R-3)

Step 1 `main.go` (line 66) and Step 4 (line 533) use a 1-arg form:

```go
// WRONG (02_implementation_steps.md):
fmtr := NewFormatter(os.Stdout)
func NewFormatter(out io.Writer) *Formatter
```

The canonical form from `01_detailed_design.md` Section 4.2 is the 2-arg form,
since errors must go to stderr:

```go
// CORRECT (01_detailed_design.md):
fmtr := NewFormatter(os.Stdout, os.Stderr)
func NewFormatter(out, errOut io.Writer) *Formatter {
    return &Formatter{
        out:          out,
        errOut:       errOut,
        displayMode:  DisplayAuto,
        outputFormat: FormatTable,
        showTiming:   true,
    }
}
```

Note: Step 4's `Formatter` struct already has both `out` and `errOut` fields
(lines 525-526), which contradicts its own 1-arg `NewFormatter`. The 2-arg
constructor resolves this internal inconsistency.

### A4. `Command` struct layout: `Keys`/`Values`/`NotExist`/`AddrArg` (R-4)

Step 3 (line 303) defines a flat `Args`/`Flags`/`StrArg` layout:

```go
// WRONG (02_implementation_steps.md):
type Command struct {
    Type    CommandType
    Args    [][]byte
    IntArg  int64
    TxnOpts []client.TxnOption
    StrArg  string
    Flags   uint32
}
```

The canonical layout from `01_detailed_design.md` Section 3.1 uses separate
`Keys`/`Values` fields for clarity:

```go
// CORRECT (01_detailed_design.md):
type Command struct {
    Type     CommandType
    Keys     [][]byte            // positional key arguments
    Values   [][]byte            // positional value arguments
    IntArg   int64               // numeric argument (limit, TTL, store/region ID)
    TxnOpts  []client.TxnOption  // BEGIN options
    NotExist bool                // CAS NOT_EXIST flag
    AddrArg  string              // STATUS <addr> argument
}
```

The parser populates `Keys` and `Values` separately. Executor handlers reference
`cmd.Keys[0]`, `cmd.Values[0]`, etc. The `Flags` bitfield is replaced by explicit
`NotExist bool`. The `StrArg` field is replaced by `AddrArg` for the STATUS
command; meta command string arguments (timing on/off, format name) are handled
by `ParseMetaCommand` which routes to dedicated `CmdMetaTiming`/`CmdMetaFormat`
types -- the REPL handles these directly without going through the executor's
`Command` struct.

### A5. `CmdGCSafePoint` canonical spelling (R-7)

Step 3 (line 289) uses `CmdGCSafepoint` (lowercase "p" in "point"). The canonical
spelling from `01_detailed_design.md` is `CmdGCSafePoint` (capital "P"). All
occurrences of `CmdGCSafepoint` in this document should read `CmdGCSafePoint`:

- Step 3 enum definition (line 289)
- Step 3 dispatch table: `GC SAFEPOINT` row (line 354)
- Step 7 handler logic table: `CmdGCSafepoint` row
- Step 7 test case `TestAdminGCSafepoint` / `TestAdminGCSafepointZero`

### A6. `ResultType` enum alignment (R-8)

Step 4 (lines 488-500) defines `ResultBegin` and `ResultCommit` but omits
`ResultMessage`. The canonical enum (per `01_detailed_design.md` Addendum A2)
includes all three:

```go
const (
    ResultOK       ResultType = iota
    ResultValue
    ResultNotFound
    ResultNil
    ResultRows
    ResultTable
    ResultScalar
    ResultCAS
    ResultChecksum
    ResultMessage    // free-form text (HELP, TSO, REGION, STORE STATUS)
    ResultBegin      // BEGIN output (startTS, mode description)
    ResultCommit     // COMMIT output (committed confirmation)
)
```

Step 4's `ResultBegin` and `ResultCommit` are retained. `ResultMessage` is added
for HELP output, TSO decomposition, REGION info, and STORE STATUS detail.

### A7. `Token` field name (R-5, informational)

Step 2 (line 144) uses `Value []byte` while `01_detailed_design.md` Section 2.2
(line 143) uses `Bytes []byte`. Since `01_detailed_design.md` is canonical, the
field name should be `Bytes`. Implementers should use `Token.Bytes` when
referencing the decoded byte value of a token.

### A8. `Result.Rows` type (R-12, informational)

Step 4 (line 505) uses `Rows []KvPairResult` with a local `KvPairResult` struct.
`01_detailed_design.md` Section 4.1 uses `Rows []client.KvPair` directly. Since
`client.KvPair` is `{Key []byte, Value []byte}` (confirmed in
`pkg/client/rawkv.go:16-19`), a local wrapper is unnecessary. Use
`[]client.KvPair` for `Result.Rows`.

### A9. `defaultScanLimit` placement (R-9, informational)

Step 4 places `pageSize` on the `Formatter` struct with `PageSize()` /
`SetPageSize()` methods. `01_detailed_design.md` places `defaultScanLimit` on the
`Executor` struct (Section 3.2, line 347). The canonical placement is on the
Executor, since the scan limit is used when calling `rawkv.Scan` and placing it
on the Formatter would create a dependency from the Executor to the Formatter.
The `\pagesize` meta command modifies `exec.defaultScanLimit` (see
`01_detailed_design.md` Section 5.5, line 1036).

### A10. Explicit E2E test step note (review STEP-2)

This document's 10 steps end at "Build Integration" (Step 10). E2E tests described
in `03_test_plan.md` Section 3 are a separate phase that follows Step 10. After
Step 10 acceptance criteria are met, implementers should proceed to write E2E tests
per `03_test_plan.md` Section 3, using `pkg/e2elib` to bootstrap a test cluster
and invoking `gookv-cli` as an external process with the `-c` flag. This can be
considered "Step 11" or a post-integration verification phase.
