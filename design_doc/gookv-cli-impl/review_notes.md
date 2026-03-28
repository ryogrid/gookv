# gookv-cli Implementation Design Review

## First Review: Implementability (2026-03-28)

Reviewed files: `01_detailed_design.md`, `02_implementation_steps.md`, `03_test_plan.md`.
Cross-referenced against the actual codebase in `pkg/client/`, `pkg/pdclient/`, `pkg/e2elib/`, `Makefile`, and `go.mod`.

### 1. Step Ordering and Circular Dependencies

The 10 steps can be followed linearly. The dependency graph at the bottom of `02_implementation_steps.md` (lines 1174-1186) is accurate: Steps 2/3/4 are independent of each other (pure parsing and formatting with no client imports), Steps 5/6/7 are independent of each other (all depend only on Step 1's scaffold), and Step 8 (REPL) is the integration point. No circular dependencies exist.

One minor ordering concern: Step 1 calls `NewExecutor(c, pdClient)` in `main.go` and `NewFormatter(os.Stdout)` in the stub, but the executor stub must accept both args to compile. This is covered by the "Stub Files" section, so no blocker.

**Verdict: PASS -- linear execution is feasible.**

### 2. Code Snippet Correctness and Completeness

#### 2a. `NewExecutor` signature mismatch

`01_detailed_design.md` line 351 defines:
```go
func NewExecutor(c *client.Client) *Executor  // 1 arg
```

`02_implementation_steps.md` line 65 calls it as:
```go
exec := NewExecutor(c, pdClient)  // 2 args
```

And Step 5 (line 640) defines:
```go
func NewExecutor(c *client.Client, pd pdclient.Client) *Executor
```

The detailed design's `NewExecutor` extracts `pdClient` via `c.PD()` internally, while the implementation steps pass it explicitly. **Pick one.** Since the `PD()` accessor is being added anyway, the 1-arg version in `01_detailed_design.md` is cleaner -- but then `main.go` in Step 1 must not pass `pdClient` as a second arg.

**Action required: reconcile to one signature. Recommend the 1-arg form (detailed design version) and update `main.go` in Step 1.**

#### 2b. `SplitStatements` return signature mismatch

`01_detailed_design.md` line 113:
```go
func SplitStatements(input string) (stmts []string, complete bool)  // 2 returns
```

`02_implementation_steps.md` line 160:
```go
func SplitStatements(input string) (stmts []string, remainder string, complete bool)  // 3 returns
```

The REPL code in `01_detailed_design.md` (line 867) uses the 2-return form. The `runBatch` in `02_implementation_steps.md` (line 948) uses the 3-return form (`stmts, _, _`). Test cases in both `02_implementation_steps.md` and `03_test_plan.md` list columns for `Remainder`, matching the 3-return form.

**Action required: pick one. The 3-return form is more explicit and matches the tests. Update the detailed design's function signature and REPL usage to match.**

#### 2c. `NewFormatter` signature mismatch

`02_implementation_steps.md` line 66 and line 533:
```go
fmtr := NewFormatter(os.Stdout)        // 1 arg
func NewFormatter(out io.Writer) *Formatter
```

`01_detailed_design.md` lines 612 and 1074:
```go
func NewFormatter(out, errOut io.Writer) *Formatter  // 2 args
fmtr := NewFormatter(os.Stdout, os.Stderr)
```

The 2-arg form is correct since the Formatter needs separate stdout/stderr writers for `FormatError`. The Step 4 definition in `02_implementation_steps.md` has a `Formatter` struct with both `out` and `errOut` fields (line 526-527), contradicting its own 1-arg `NewFormatter`.

**Action required: use the 2-arg form consistently.**

#### 2d. `Token` field name: `Bytes` vs `Value`

`01_detailed_design.md` line 143: `Bytes []byte`
`02_implementation_steps.md` line 144: `Value []byte`

The test tables in `02_implementation_steps.md` reference "Expected Tokens" as strings and check the decoded byte value, but the field name is inconsistent across documents.

**Action required: pick one name. `Value` is more conventional for a parser token.**

#### 2e. `Command` struct field divergence

`01_detailed_design.md` (line 315) uses `Keys [][]byte`, `Values [][]byte`, `NotExist bool`, `AddrArg string`.
`02_implementation_steps.md` (line 303) uses a flat `Args [][]byte`, `Flags uint32`, `StrArg string`.

These are two different designs for the same struct. The flat `Args` approach is simpler but pushes key/value separation logic into the executor. The separate `Keys`/`Values` approach is more self-documenting. The executor code snippets in `01_detailed_design.md` (lines 441, 452) reference `cmd.Keys[0]`, while the Step 5 handler table just says "key"/"val" generically.

**Action required: pick one layout. The flat `Args` approach (from Step 3) is simpler for a parser to fill, but the executor code referencing `cmd.Keys[0]` must be updated to `cmd.Args[0]`.**

#### 2f. `handleMetaCommand` is missing `\x` and `\t` cases

The `handleMetaCommand` function in `01_detailed_design.md` (lines 976-1041) has case arms for `\help`, `\h`, `\?`, `\quit`, `\q`, `\timing`, `\format`, `\pagesize`, but NOT for `\x` (cycle display mode) or `\t` (short form for `\timing` toggle). Both are listed as supported in the help text, the meta command parser tests, and `ParseMetaCommand`.

**Action required: add `case "\x":` and `case "\t":` to the switch in `handleMetaCommand`.**

#### 2g. `CmdGCSafePoint` vs `CmdGCSafepoint` naming

`01_detailed_design.md` (line 306): `CmdGCSafePoint` (capital P)
`02_implementation_steps.md` (line 289): `CmdGCSafepoint` (lowercase p)

This will cause a compile error if both documents are used as reference.

**Action required: standardize to one spelling. Go convention favors `CmdGCSafepoint` (treat "Safepoint" as one word).**

#### 2h. `ResultType` enum drift

`01_detailed_design.md` (lines 549-561) defines: `ResultOK`, `ResultValue`, `ResultNotFound`, `ResultNil`, `ResultRows`, `ResultTable`, `ResultScalar`, `ResultCAS`, `ResultChecksum`, `ResultMessage`.
`02_implementation_steps.md` (lines 488-500) defines a different set: adds `ResultBegin`, `ResultCommit`, removes `ResultMessage`.

The executor uses `ResultMessage` (free-form) for HELP and TSO output in the detailed design. The implementation steps use `ResultBegin` and `ResultCommit` for transaction lifecycle output.

**Action required: merge both sets. The executor needs `ResultBegin`, `ResultCommit`, AND `ResultMessage` (or unify via `ResultScalar`). Decide which result type HELP uses.**

#### 2i. `Result` struct field drift

`01_detailed_design.md` `Result` uses `Rows []client.KvPair`.
`02_implementation_steps.md` `Result` uses `Rows []KvPairResult` (a separate local struct).

Since `client.KvPair` is `{Key []byte, Value []byte}` (confirmed in `pkg/client/rawkv.go:16-19`), a local `KvPairResult` wrapper is unnecessary unless additional fields are needed. Using `client.KvPair` directly is simpler.

**Action required: use `client.KvPair` consistently, or justify `KvPairResult`.**

### 3. Parser Ambiguities

The parser design is well-specified. Three minor ambiguities remain:

**3a. REGION keyword collision**: `REGION LIST` and `REGION ID` are two-word commands, but `REGION mykey` is a single-word command with a key argument. If the key happens to be the literal string `LIST` or `ID`, the parser will misinterpret it as a subcommand. The design does not mention how to escape this (e.g., quoting: `REGION "LIST"`). Since `Tokenize` strips quotes and the command parser checks the string value of the second token, `REGION "LIST"` would still match the `LIST` keyword unless the parser compares against the `Raw` field instead of the decoded `Value`/`Bytes`.

**Action required: document that `REGION "LIST"` looks up the region for the key "LIST" (check the Raw field for quotes), OR accept this as a known limitation and document it.**

**3b. `DELETE RANGE` vs `DELETE` with key "RANGE"**: Same keyword collision pattern. If a user has a key literally named `RANGE`, `DELETE RANGE` will be parsed as `CmdDeleteRange` instead of `CmdDelete` for key "RANGE". Quoting (`DELETE "RANGE"`) would need the same Raw-field check.

**Action required: document the behavior or add a quoting-based disambiguation.**

**3c. `DecodeValue` function**: Listed in `02_implementation_steps.md` (line 169) as part of the parser API but never referenced by `ParseCommand` or the executor. `Tokenize` already decodes hex and handles quotes, so `DecodeValue` appears redundant.

**Action required: remove `DecodeValue` from the API surface, or clarify its intended use case (perhaps for interactive display formatting).**

### 4. Test Infrastructure Feasibility

**4a. Mock interfaces**: The design proposes `rawKVAPI` and `txnKVAPI` interfaces for mock-based testing. Verified against the codebase:
- `rawKVAPI` (Step 5, lines 680-693): all 12 methods exist on `RawKVClient` with matching signatures. **PASS.**
- `txnKVAPI` (Step 6, line 752-754): `Begin` method exists on `TxnKVClient`. **PASS.**
- `txnHandleAPI` (test plan, lines 277-285): `TxnHandle` has `StartTS`, `Get`, `BatchGet`, `Set`, `Delete`, `Commit`, `Rollback` -- all confirmed. **PASS.**

**4b. `pdclient.MockClient`**: Exists at `pkg/pdclient/mock.go`. It implements `GetAllStores`, `GetStore`, `GetRegion`, `GetRegionByID`, `GetTS`, `GetGCSafePoint`, `GetClusterID`. All admin commands in Step 7 can be tested against it. **PASS.**

**4c. E2E with `e2elib`**: The project has an established `pkg/e2elib/` package and `e2e_external/` test directory. The proposed `e2e_external/cli/cli_test.go` pattern (spawn `gookv-cli` as an external process with `-c` flag) is consistent with how existing E2E tests work. However, E2E tests invoke the binary by path, so the binary must be built first -- the test plan's helper `runCLI` would need to locate the built `gookv-cli` binary. The `pkg/e2elib/binary.go` file likely handles binary discovery.

One concern: the E2E tests propose running `gookv-cli` with `-c` batch mode against a real cluster, but transaction E2E tests like `TestE2ETxnRoundTrip` send `BEGIN; SET k1 v1; ... COMMIT; GET k1;` -- the final `GET k1;` is a raw GET (outside the transaction). This works because batch mode processes statements sequentially with shared executor state. **PASS.**

**4d. Ctrl-C testing (regression 4.4)**: The design correctly notes these are "manual/integration tests" and not programmatically testable. This is honest and acceptable.

### 5. External Dependencies

**5a. `github.com/chzyer/readline v1.5.1`**: Exists on Go module proxy (confirmed: `v1.5.0` and `v1.5.1` available). Compatible with Go 1.25. The project already uses `go 1.25.0`. **PASS.**

However, `chzyer/readline` is in maintenance mode and has known issues on some terminal types. A more actively maintained fork is `github.com/ergochat/readline`. This is a non-blocking note.

**5b. `github.com/olekukonko/tablewriter v0.0.5`**: Exists (confirmed). However, `v1.0.0` through `v1.1.4` are also available. The `v0.0.5` API uses `tablewriter.NewWriter(io.Writer)` which matches the code snippets. The `v1.x` API has breaking changes. **PASS -- v0.0.5 is correct for the snippets shown.**

Note: `v0.0.5` is quite old (2020). Consider `v0.0.5` for stability or upgrading the snippets to `v1.x` for long-term maintenance. Non-blocking.

---

## Second Review: External Spec Compliance (2026-03-28)

Reviewer: Claude Opus 4.6
Verified against: External spec (`design_doc/gookv-cli/01_overview.md`, `02_command_reference.md` including all addenda), `pkg/client/` source, `pkg/pdclient/` source

---

### 1. Command Coverage: Does 01_detailed_design.md implement ALL commands?

Checked the complete command set from `design_doc/gookv-cli/02_command_reference.md`
(including all addenda A1--A5) against the implementation design.

#### Raw KV Commands (12/12 -- Complete)

| # | Command | In 02_command_reference.md | In 01_detailed_design.md | Status |
|---|---------|---------------------------|--------------------------|--------|
| 1 | GET | Section 2.1 | CmdGet + execRawGet | OK |
| 2 | PUT | Section 2.2 | CmdPut + execRawPut | OK |
| 3 | PUT TTL | Section 2.2 | CmdPutTTL + execRawPutTTL | OK |
| 4 | DELETE | Section 2.3 | CmdDelete + execRawDelete | OK |
| 5 | TTL | Section 2.4 | CmdTTL + execRawTTL | OK |
| 6 | SCAN | Section 2.5 | CmdScan + execRawScan | OK |
| 7 | BGET | Section 2.6 | CmdBatchGet + execRawBatchGet | OK |
| 8 | BPUT | Section 2.7 | CmdBatchPut + execRawBatchPut | OK |
| 9 | BDELETE | Section 2.8 | CmdBatchDelete + execRawBatchDelete | OK |
| 10 | DELETE RANGE | Section 2.9 | CmdDeleteRange + execRawDeleteRange | OK |
| 11 | CAS | Section 2.10 | CmdCAS + execRawCAS | OK |
| 12 | CHECKSUM | Section 2.11 | CmdChecksum + execRawChecksum | OK |

#### Transactional Commands (7/7 -- Complete)

| # | Command | In 02_command_reference.md | In 01_detailed_design.md | Status |
|---|---------|---------------------------|--------------------------|--------|
| 1 | BEGIN | Section 3.1 | CmdBegin + execBegin | OK |
| 2 | GET (txn) | Section 3.3, Addendum A1 | CmdTxnGet + execTxnGet | OK |
| 3 | BGET (txn) | Addendum A1 | CmdTxnBatchGet + execTxnBatchGet | OK |
| 4 | SET | Section 3.2 | CmdTxnSet + execTxnSet | OK |
| 5 | DELETE (txn) | Section 3.4, Addendum A1 | CmdTxnDelete + execTxnDelete | OK |
| 6 | COMMIT | Section 3.5 | CmdCommit + execCommit | OK |
| 7 | ROLLBACK | Section 3.6 | CmdRollback + execRollback | OK |

#### Administrative Commands (9/9 -- Complete)

| # | Command | In 02_command_reference.md | In 01_detailed_design.md | Status |
|---|---------|---------------------------|--------------------------|--------|
| 1 | STORE LIST | Section 4.1 | CmdStoreList + execStoreList | OK |
| 2 | STORE STATUS | Section 4.2 | CmdStoreStatus + execStoreStatus | OK |
| 3 | REGION | Section 4.3 | CmdRegion + execRegion | OK |
| 4 | REGION LIST | Section 4.4 | CmdRegionList + execRegionList | OK |
| 5 | REGION ID | Addendum A2 | CmdRegionByID + execRegionByID | OK |
| 6 | CLUSTER INFO | Section 4.5 | CmdClusterInfo + execClusterInfo | OK |
| 7 | TSO | Section 4.6, Addendum A5 | CmdTSO + execTSO | OK |
| 8 | GC SAFEPOINT | Section 4.7, Addendum A5 | CmdGCSafePoint + execGCSafePoint | OK |
| 9 | STATUS | Section 4.8, Addendum A5 | CmdStatus + execStatus | OK |

#### Meta Commands (7/7 -- Complete)

| # | Command | In 02_command_reference.md | In 01_detailed_design.md | Status |
|---|---------|---------------------------|--------------------------|--------|
| 1 | \help / \h / \? / HELP; | Addendum A1 | CmdHelp + execHelp | OK |
| 2 | \quit / \q / EXIT; / QUIT; | Addendum A1 | CmdExit + execExit | OK |
| 3 | \timing on/off / \t | Addendum A1 | CmdMetaTiming | OK |
| 4 | \format table/plain/hex | Addendum A1 | CmdMetaFormat | OK |
| 5 | \pagesize N | Addendum A1 | CmdMetaPagesize | OK |
| 6 | \x | Addendum A1 | CmdMetaHex | OK |
| 7 | --version flag | Addendum A8 | main.go --version | OK |

**Verdict: All commands from the external spec (including all addenda) are
covered in the implementation design. No missing commands.**

---

### 2. Go Type Accuracy: Do types in 01_detailed_design.md match pkg/client/?

Verified every API call in the design against the actual Go source.

#### Confirmed Matches

| Design Doc Reference | Actual Signature | Match |
|---|---|---|
| `RawKVClient.Get(ctx, key) ([]byte, bool, error)` | `func (c *RawKVClient) Get(ctx context.Context, key []byte) ([]byte, bool, error)` | Yes |
| `RawKVClient.Put(ctx, key, value) error` | `func (c *RawKVClient) Put(ctx context.Context, key, value []byte) error` | Yes |
| `RawKVClient.PutWithTTL(ctx, key, value, ttl) error` | `func (c *RawKVClient) PutWithTTL(ctx context.Context, key, value []byte, ttl uint64) error` | Yes |
| `RawKVClient.Delete(ctx, key) error` | `func (c *RawKVClient) Delete(ctx context.Context, key []byte) error` | Yes |
| `RawKVClient.GetKeyTTL(ctx, key) (uint64, error)` | `func (c *RawKVClient) GetKeyTTL(ctx context.Context, key []byte) (uint64, error)` | Yes |
| `RawKVClient.Scan(ctx, start, end, limit) ([]KvPair, error)` | `func (c *RawKVClient) Scan(ctx context.Context, startKey, endKey []byte, limit int) ([]KvPair, error)` | Yes |
| `RawKVClient.BatchGet(ctx, keys) ([]KvPair, error)` | `func (c *RawKVClient) BatchGet(ctx context.Context, keys [][]byte) ([]KvPair, error)` | Yes |
| `RawKVClient.BatchPut(ctx, pairs) error` | `func (c *RawKVClient) BatchPut(ctx context.Context, pairs []KvPair) error` | Yes |
| `RawKVClient.BatchDelete(ctx, keys) error` | `func (c *RawKVClient) BatchDelete(ctx context.Context, keys [][]byte) error` | Yes |
| `RawKVClient.DeleteRange(ctx, start, end) error` | `func (c *RawKVClient) DeleteRange(ctx context.Context, startKey, endKey []byte) error` | Yes |
| `RawKVClient.CompareAndSwap(ctx, key, value, prevValue, prevNotExist) (bool, []byte, error)` | `func (c *RawKVClient) CompareAndSwap(ctx context.Context, key, value, prevValue []byte, prevNotExist bool) (bool, []byte, error)` | Yes |
| `RawKVClient.Checksum(ctx, start, end) (uint64, uint64, uint64, error)` | `func (c *RawKVClient) Checksum(ctx context.Context, startKey, endKey []byte) (uint64, uint64, uint64, error)` | Yes |
| `TxnKVClient.Begin(ctx, opts...) (*TxnHandle, error)` | `func (c *TxnKVClient) Begin(ctx context.Context, opts ...TxnOption) (*TxnHandle, error)` | Yes |
| `TxnHandle.Get(ctx, key) ([]byte, error)` | `func (t *TxnHandle) Get(ctx context.Context, key []byte) ([]byte, error)` | Yes |
| `TxnHandle.BatchGet(ctx, keys) ([]KvPair, error)` | `func (t *TxnHandle) BatchGet(ctx context.Context, keys [][]byte) ([]KvPair, error)` | Yes |
| `TxnHandle.Set(ctx, key, value) error` | `func (t *TxnHandle) Set(ctx context.Context, key, value []byte) error` | Yes |
| `TxnHandle.Delete(ctx, key) error` | `func (t *TxnHandle) Delete(ctx context.Context, key []byte) error` | Yes |
| `TxnHandle.Commit(ctx) error` | `func (t *TxnHandle) Commit(ctx context.Context) error` | Yes |
| `TxnHandle.Rollback(ctx) error` | `func (t *TxnHandle) Rollback(ctx context.Context) error` | Yes |
| `TxnHandle.StartTS() txntypes.TimeStamp` | `func (t *TxnHandle) StartTS() txntypes.TimeStamp` | Yes |
| `WithPessimistic() TxnOption` | `func WithPessimistic() TxnOption` | Yes |
| `WithAsyncCommit() TxnOption` | `func WithAsyncCommit() TxnOption` | Yes |
| `With1PC() TxnOption` | `func With1PC() TxnOption` | Yes |
| `WithLockTTL(ttl uint64) TxnOption` | `func WithLockTTL(ttl uint64) TxnOption` | Yes |
| `pdclient.Client.GetTS(ctx) (TimeStamp, error)` | Interface method, returns `pdclient.TimeStamp` | Yes |
| `pdclient.Client.GetRegion(ctx, key) (*metapb.Region, *metapb.Peer, error)` | Interface method | Yes |
| `pdclient.Client.GetRegionByID(ctx, regionID) (*metapb.Region, *metapb.Peer, error)` | `regionID` is `uint64` | Yes |
| `pdclient.Client.GetStore(ctx, storeID) (*metapb.Store, error)` | `storeID` is `uint64` | Yes |
| `pdclient.Client.GetAllStores(ctx) ([]*metapb.Store, error)` | Interface method | Yes |
| `pdclient.Client.GetGCSafePoint(ctx) (uint64, error)` | Interface method | Yes |
| `pdclient.Client.GetClusterID(ctx) uint64` | Returns `uint64` (no error) | Yes |

#### Issues Found

**[API-1] `Client` has no `PD()` accessor yet (Expected -- design addresses this)**
The design doc correctly identifies this gap and specifies adding:
```go
func (c *Client) PD() pdclient.Client { return c.pdClient }
```
to `pkg/client/client.go`. This is documented in Section 1 of 01_detailed_design.md
and Step 1 of 02_implementation_steps.md. No issue.

**[API-2] `NewExecutor` signature inconsistency between 01_detailed_design.md and 02_implementation_steps.md (Low)**
- `01_detailed_design.md` Section 3.2: `NewExecutor(c *client.Client) *Executor` -- takes one arg,
  calls `c.PD()` internally.
- `02_implementation_steps.md` Step 1: `NewExecutor(c, pdClient)` -- takes two args.
- `02_implementation_steps.md` Step 5: `NewExecutor(c *client.Client, pd pdclient.Client)` -- two args.

The single-arg form (01) is cleaner since the `PD()` accessor provides the PD client.
The two-arg form (02) is redundant given the accessor exists. Pick one. The single-arg
form from 01_detailed_design.md is better.

**[API-3] `NewFormatter` signature inconsistency (Low)**
- `01_detailed_design.md` Section 4.2: `NewFormatter(out, errOut io.Writer)` -- two writers.
- `02_implementation_steps.md` Step 1: `NewFormatter(os.Stdout)` -- one writer.
- `02_implementation_steps.md` Step 4: `NewFormatter(out io.Writer)` -- one writer.
- `01_detailed_design.md` Section 5.6 main.go: `NewFormatter(os.Stdout, os.Stderr)` -- two writers.

The two-writer form is correct since errors go to stderr and results go to stdout.
Step 4 of 02_implementation_steps.md should use two writers.

**[API-4] `Formatter.pageSize` field placement (Low)**
- `01_detailed_design.md` Section 3.1: `defaultScanLimit` is on the `Executor` struct.
- `02_implementation_steps.md` Step 4: `pageSize` is on the `Formatter` struct, with
  `PageSize()` and `SetPageSize()` methods.

These are contradictory. The scan default limit logically belongs on the Executor
(which calls `rawkv.Scan` with the limit). Having it on the Formatter would require
the Formatter to be passed to the Executor, creating a circular dependency.
01_detailed_design.md (Executor.defaultScanLimit) is the correct placement.

**Verdict: All API signatures match the actual source code. The design correctly
maps every command to the right Go method with the right parameter types. The
minor inconsistencies are between the two implementation docs, not between the
implementation design and the actual code.**

---

### 3. Implementation Steps Coverage: Does 02_implementation_steps.md cover every component?

#### Components from 01_detailed_design.md

| Component | Section in 01 | Step in 02 | Status |
|---|---|---|---|
| main.go (flags, client init, mode dispatch) | Section 1, 5.6 | Step 1 | OK |
| PD() accessor on Client | Section 1 | Step 1 | OK |
| Makefile update | Section 1 | Step 1, 10 | OK |
| Statement splitter (SplitStatements) | Section 2.1 | Step 2 | OK |
| Tokenizer (Tokenize, Token) | Section 2.2 | Step 2 | OK |
| Meta command detection (IsMetaCommand) | -- | Step 2 | OK |
| Command parser (ParseCommand) | Section 2.3 | Step 3 | OK |
| Meta command parser (ParseMetaCommand) | -- | Step 3 | OK |
| CommandType enum | Section 3.1 | Step 3 | OK |
| Command struct | Section 3.1 | Step 3 | OK |
| Executor struct + handler registry | Section 3.2 | Step 5 | OK |
| Context-sensitive dispatch | Section 3.3 | Step 3 (parser) | OK |
| Not-found semantics (raw vs txn) | Section 3.4 | Step 5 (raw), Step 6 (txn) | OK |
| Transaction state machine | Section 3.5 | Step 6 | OK |
| Per-command context for Ctrl-C | Section 3.6 | Step 8 | OK |
| Result types | Section 4.1 | Step 4 | OK |
| Formatter struct | Section 4.2 | Step 4 | OK |
| Display helpers (formatBytes, isPrintableASCII) | Section 4.3 | Step 4 | OK |
| Table rendering | Section 4.5 | Step 4 | OK |
| REPL loop (readline, prompt, history) | Section 5.1 | Step 8 | OK |
| Prompt state machine | Section 5.2 | Step 8 | OK |
| Stdin pipe detection | Section 5.3 | Step 8 | OK |
| Batch mode (-c flag) | Section 5.4 | Step 8 | OK |
| Backslash meta command handling | Section 5.5 | Step 9 | OK |
| Error handling (categories, display) | Section 6 | Step 5, 6, 8 | OK |
| Raw KV handlers (12 commands) | Appendix A | Step 5 | OK |
| Transaction handlers (7 commands) | Appendix A | Step 6 | OK |
| Admin handlers (9 commands) | Appendix A | Step 7 | OK |
| Meta command handlers | Appendix A | Step 9 | OK |
| TSO decomposition | Appendix B | Step 7 | OK |
| DELETE RANGE confirmation | Appendix C | Step 5 (noted), Step 8 | OK |
| go.mod dependencies | Section 1 | Step 10 | OK |

#### Missing/Incomplete Items

**[STEP-1] Step 2 SplitStatements returns 3 values but 01_detailed_design.md returns 2 (Low)**
See First Review item 2b. The 3-return form is better. Update 01.

**[STEP-2] No explicit step for E2E test setup (Medium)**
02_implementation_steps.md has 10 steps ending at "Build Integration." The E2E tests
described in 03_test_plan.md Section 3 require a test cluster and `e2elib`, but there
is no step in 02 for writing E2E tests. This is acceptable if E2E tests are considered
a separate phase, but it should be noted.

**[STEP-3] ResultBegin and ResultCommit types in 02 but not in 01 (Low)**
02_implementation_steps.md Step 4 defines `ResultBegin` and `ResultCommit` as
separate ResultType values. 01_detailed_design.md Section 4.1 does not have these --
it uses `ResultMessage` and `ResultOK` for BEGIN and COMMIT outputs respectively.
The Step 4 approach (dedicated types) is slightly more structured but either works.

**Verdict: 02_implementation_steps.md covers all components from 01_detailed_design.md.
The dependency ordering (Steps 1-10) is sound. Minor signature inconsistencies
exist between the two docs.**

---

### 4. Test Plan Coverage: Does 03_test_plan.md cover every command?

#### Raw KV Commands

| Command | Unit Test (Parser) | Integration Test (Executor) | E2E Test | Status |
|---|---|---|---|---|
| GET | #1, #2 (parser) | #1-3 (found/notfound/error) | #1 | OK |
| PUT | #3, #4 (parser) | #4-5 (success/error) | #1, #2 | OK |
| PUT TTL | #4 (parser) | #6 | #2 | OK |
| DELETE | #6, #7, #8 (parser) | #7 | #3 | OK |
| TTL | #12 (parser) | #8-9 | #2 | OK |
| SCAN | #13, #14 (parser) | #10-11 | #4, #5 | OK |
| BGET | #16, #17 (parser) | #12 | #6 | OK |
| BPUT | #18, #19 (parser) | #13 | #7 | OK |
| BDELETE | #20, #21 (parser) | #14 | #8 | OK |
| DELETE RANGE | #8, #9 (parser) | #15 | #9 | OK |
| CAS | #22-24 (parser) | #16-18 | #10-11 | OK |
| CHECKSUM | #25-27 (parser) | #19 | #12 | OK |

#### Transactional Commands

| Command | Unit Test (Parser) | Integration Test (Executor) | E2E Test | Status |
|---|---|---|---|---|
| BEGIN | #28-32 (parser) | #1-4 (lifecycle) | #1, #4 | OK |
| SET (txn) | #10-11 (parser) | #5-6 | #1 | OK |
| GET (txn) | #2 (parser) | #7-8 | #3 | OK |
| DELETE (txn) | #7 (parser) | #9 | #6 | OK |
| BGET (txn) | #17 (parser) | #10 | #5 | OK |
| COMMIT | #33 (parser) | #11-13 | #1 | OK |
| ROLLBACK | #34 (parser) | #14-16 | #2 | OK |

#### Administrative Commands

| Command | Unit Test (Parser) | Integration Test (Executor) | E2E Test | Status |
|---|---|---|---|---|
| STORE LIST | #35 (parser) | #1-2 (admin) | #1 | OK |
| STORE STATUS | #36-38 (parser) | #3-4 (admin) | #2 | OK |
| REGION | #39, #43 (parser) | #5 (admin) | #3 | OK |
| REGION LIST | #40-41 (parser) | #6-7 (admin) | #4 | OK |
| REGION ID | #42 (parser) | #8-9 (admin) | -- | **Gap: no E2E** |
| CLUSTER INFO | #44-45 (parser) | #10 (admin) | #5 | OK |
| TSO | #46 (parser) | #11-12 (admin) | #6 | OK |
| GC SAFEPOINT | #47-48 (parser) | #13-14 (admin) | #7 | OK |
| STATUS | #49-50 (parser) | -- | -- | **Gap: no executor/E2E** |

#### Meta Commands

| Command | Unit Test (Meta Parser) | Integration/Regression | E2E Test | Status |
|---|---|---|---|---|
| \help / HELP | #1-3 (meta parser), #51 (parser) | Regression 4.1 | #1 | OK |
| \quit / EXIT / QUIT | #4-5 (meta parser), #52-53 (parser) | Regression 4.1 | #2 | OK |
| \timing on/off / \t | #6-9 (meta parser) | Step 9 tests | -- | OK (unit) |
| \format table/plain/hex | #10-12 (meta parser) | Step 9 tests | -- | OK (unit) |
| \pagesize N | #14-16 (meta parser) | Regression 4.5 | -- | OK (unit) |
| \x | #17 (meta parser) | Step 9 tests | -- | OK (unit) |

#### Issues Found

**[TEST-1] STATUS command has no executor test or E2E test (Medium)**
The `STATUS` command (HTTP GET to a server's status endpoint) appears in the parser
test cases (#49, #50) but has no executor integration test and no E2E test. Since this
command involves HTTP (not gRPC like all other commands), it needs dedicated testing.

**[TEST-2] REGION ID has no E2E test (Low)**
The `REGION ID` command has parser tests (#42) and mock-based executor tests (#8-9)
but no E2E test in Section 3.4. Adding one is straightforward: after bootstrapping a
cluster, get a region's ID from `REGION LIST` and then verify `REGION ID <id>` returns
the same region info.

**[TEST-3] No E2E test for \pagesize (Low)**
The `\pagesize` meta command is tested at the unit level (parser + regression 4.5
cases) but not at the E2E level. Since `\pagesize` affects SCAN behavior and backslash
meta commands are REPL-only (not usable with `-c` batch mode), E2E testing is not
directly possible. Acceptable to skip.

**[TEST-4] Missing test for SCAN in transaction (Medium)**
The external spec says `TxnHandle` has no `Scan` method, and regression test 4.7 #3
tests `SCAN in txn` expecting an error. But 03_test_plan.md's executor integration
tests (Section 2.2) do not include a case for "SCAN attempted inside a transaction."
The regression test covers it, but an explicit executor test case would be clearer.

**Verdict: 03_test_plan.md covers every command from the external spec at the parser
level. Two commands (STATUS and REGION ID) have test coverage gaps at the
executor/E2E level. Overall coverage is thorough with ~235 test cases.**

---

### 5. Command Name Consistency with Addenda

Checked that the implementation design uses the canonical command names established
by the addenda in `design_doc/gookv-cli/01_overview.md` (Sections A1--A8) and
`design_doc/gookv-cli/02_command_reference.md` (Sections A1--A5).

| Naming Decision | Canonical (from addenda) | In 01_detailed_design.md | In 02_implementation_steps.md | In 03_test_plan.md | Status |
|---|---|---|---|---|---|
| Batch delete command | `BDELETE` (not `BDEL`) | `CmdBatchDelete`, `BDELETE` in parser flowchart | `BDELETE` in dispatch table | `BDELETE` in test cases | OK |
| Delete range command | `DELETE RANGE` (not `DELRANGE`) | `CmdDeleteRange`, `DELETE RANGE` in parser | `DELETE RANGE` in dispatch table | `DELETE RANGE` in test cases | OK |
| SCAN limit syntax | `LIMIT` keyword required | `SCAN a z LIMIT 50` in examples | `SCAN a z LIMIT 50` in dispatch | `SCAN a z LIMIT 50` in tests | OK |
| Meta commands use backslash | `\timing`, `\format`, `\pagesize` (not `SET TIMING`, `SET FORMAT`) | `\timing`, `\format`, `\pagesize` in Section 5.5 | `\timing`, `\format`, `\pagesize` in Step 9 | `\timing`, `\format`, `\pagesize` in Section 1.5 | OK |
| Txn commands context-sensitive | `GET`/`SET`/`DELETE` (not `TGET`/`TSET`/`TDEL`) | `CmdTxnGet` etc. (internal), user types `GET` | Context-sensitive dispatch in Step 3 | `GET mykey` with inTxn=true in tests | OK |
| BEGIN options | `PESSIMISTIC`, `ASYNC_COMMIT`, `ONE_PC`, `LOCK_TTL` | Correct in Section 2.3 flowchart | Correct in Step 3 dispatch table | Correct in parser test #30 | OK |
| REGION ID command | `REGION ID <id>` | `CmdRegionByID` in parser flowchart | `REGION ID` in dispatch table | `REGION ID 42` in parser test #42 | OK |
| \pagesize command | `\pagesize <n>` | Section 5.5 handler | Step 9 handler | Meta parser test #14 | OK |
| Default SCAN limit | 100 (not 20) | `defaultScanLimit: 100` in Executor | Step 5: "limit defaults to formatter.PageSize()" | Regression 4.5 #1: "Returns 100" | OK |

**Verdict: All command names in the implementation design are consistent with the
addenda. No stale names (BDEL, DELRANGE, TGET/TSET/TDEL, SET TIMING, etc.) appear
in any of the three implementation documents.**

---

### 6. Consolidated Issue Summary

#### Must Fix (from both reviews)

| ID | Severity | Issue | Source |
|----|----------|-------|--------|
| R-1 | **High** | `NewExecutor` signature: 1-arg (01) vs 2-arg (02). Pick the 1-arg form. | First Review 2a |
| R-2 | **High** | `SplitStatements` return: 2-return (01) vs 3-return (02). Pick the 3-return form. | First Review 2b |
| R-3 | **High** | `NewFormatter` signature: 1-arg (02) vs 2-arg (01). Use 2-arg everywhere. | First Review 2c |
| R-4 | **High** | `Command` struct: `Keys`/`Values`/`NotExist`/`AddrArg` (01) vs `Args`/`Flags`/`StrArg` (02). Pick one. | First Review 2e |

#### Should Fix

| ID | Severity | Issue | Source |
|----|----------|-------|--------|
| R-5 | **Medium** | `Token.Bytes` (01) vs `Token.Value` (02) field name. Pick one. | First Review 2d |
| R-6 | **Medium** | `handleMetaCommand` missing `\x` and `\t` case arms. | First Review 2f |
| R-7 | **Medium** | `CmdGCSafePoint` (01) vs `CmdGCSafepoint` (02) naming. Standardize. | First Review 2g |
| R-8 | **Medium** | `ResultType` enum sets differ. Merge into one canonical set. | First Review 2h |
| R-9 | **Medium** | `defaultScanLimit` on Executor (01) vs `pageSize` on Formatter (02). Use Executor. | Second Review API-4 |
| R-10 | **Medium** | STATUS command has no executor test or E2E test. | Second Review TEST-1 |
| R-11 | **Medium** | Missing executor test for SCAN inside transaction (should error). | Second Review TEST-4 |

#### Low Priority

| ID | Severity | Issue | Source |
|----|----------|-------|--------|
| R-12 | Low | `Result.Rows` type: `[]client.KvPair` (01) vs `[]KvPairResult` (02). | First Review 2i |
| R-13 | Low | `DecodeValue` function appears unused. Remove or justify. | First Review 3c |
| R-14 | Low | REGION/DELETE keyword collision with literal key names. Document. | First Review 3a, 3b |
| R-15 | Low | REGION ID has no E2E test. | Second Review TEST-2 |
| R-16 | Low | No explicit E2E test step in 02_implementation_steps.md. | Second Review STEP-2 |

### Overall Assessment

The implementation design is thorough, well-structured, and closely aligned with
the external specification. All 35 commands (12 raw KV + 7 txn + 9 admin + 7 meta)
are fully covered. Go type mappings are accurate against the actual source code.
The three implementation documents are internally consistent on command names and
addenda compliance. The main issues are cross-document type/signature inconsistencies
between 01_detailed_design.md and 02_implementation_steps.md that should be
reconciled before implementation begins.

**Ready for implementation after addressing the 4 must-fix items (R-1 through R-4).**
