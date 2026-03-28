# gookv-cli Design Specification Review

Reviewer: Claude (automated review)
Date: 2026-03-28
Files reviewed: `01_overview.md`, `02_command_reference.md`, `03_implementation_plan.md`
Source verified against: `pkg/client/rawkv.go`, `pkg/client/txn.go`, `pkg/client/txnkv.go`, `pkg/client/client.go`, `pkg/pdclient/client.go`

---

## 1. Usability

### Strengths

- The semicolon-terminated, case-insensitive command syntax will feel immediately familiar to anyone coming from psql or mysql. Good choice.
- Multi-line continuation with a shifted prompt (`     > `) mirrors psql exactly. The transaction-aware prompt (`gookv(txn)>`) is a nice touch that redis-cli lacks.
- The `0x` and `x'...'` hex literal syntax for binary data is well thought out and matches conventions in tikv-ctl and MySQL's hex strings.
- Ctrl-C discards partial input without exiting, Ctrl-D exits -- matches psql and redis-cli behavior. Good.
- The DELETE RANGE confirmation prompt for full-keyspace deletes (`"" ""`) is a sensible safety net.

### Issues

**[U-1] Overloaded GET/DELETE/SET creates parsing ambiguity (Medium)**
The EBNF in `02_command_reference.md` redefines `GET`, `DELETE`, and `SET` to dispatch based on transaction context: `GET` calls `RawKVClient.Get` outside a transaction and `TxnHandle.Get` inside one. Meanwhile, `01_overview.md` uses explicit `TGET`/`TSET`/`TDEL` prefixes for the transactional variants.

These two documents contradict each other. The EBNF approach (context-sensitive dispatch) is more ergonomic for users -- typing `GET` inside a transaction is natural. But `01_overview.md` tables list `TGET`/`TSET`/`TDEL` as separate commands, and `03_implementation_plan.md` defines `CmdTGet`, `CmdTSet`, `CmdTDelete` as distinct command types.

**Recommendation:** Settle on one approach. The context-sensitive approach from `02_command_reference.md` (where `GET` "just works" in both modes) is better UX. If you keep the `T`-prefixed names, they should be aliases, not the primary names. Update `01_overview.md` Section 6.2 and `03_implementation_plan.md` to match.

**[U-2] SCAN syntax inconsistency between documents (Low)**
- `01_overview.md` says: `SCAN <start> <end> [limit]` (limit is a bare positional integer)
- `02_command_reference.md` says: `SCAN <start> <end> [LIMIT <n>]` (limit is keyword-prefixed)

The keyword-prefixed form (`LIMIT 5`) is clearer and consistent with SQL. The bare positional form (`SCAN a z 5`) is more concise. Pick one and update both documents.

**[U-3] Batch command naming inconsistency between documents (Low)**
- `01_overview.md` Section 6.1: `BGET`, `BPUT`, `BDEL`
- `02_command_reference.md` EBNF: `BGET`, `BPUT`, `BDELETE` (not `BDEL`)
- `03_implementation_plan.md`: `CmdBatchGet`, `CmdBatchPut`, `CmdBatchDelete`

`BDEL` vs `BDELETE` -- pick one. Since `DELETE` is the full raw KV command name, `BDELETE` is more consistent. Either way, update all three docs to agree.

**[U-4] DELRANGE vs DELETE RANGE (Low)**
- `01_overview.md` Section 6.1 table: `DELRANGE <start> <end>`
- `02_command_reference.md`: `DELETE RANGE <start> <end>`

`DELETE RANGE` (two words) reads more naturally and is what `02_command_reference.md` uses. Update `01_overview.md` to match.

**[U-5] No way to quote keys/values with colons or special characters without quotes (Minor)**
Unquoted tokens can contain colons (e.g., `user:001`), which is good. But the EBNF says unquoted tokens cannot contain semicolons, spaces, or double-quotes. This is fine but worth calling out in the HELP output so users know when they need quotes.

---

## 2. Completeness

### RawKVClient Methods (12 public methods)

| # | Method | Covered? | Command |
|---|--------|----------|---------|
| 1 | `Get` | Yes | `GET` |
| 2 | `Put` | Yes | `PUT` |
| 3 | `PutWithTTL` | Yes | `PUT ... TTL` |
| 4 | `Delete` | Yes | `DELETE` |
| 5 | `GetKeyTTL` | Yes | `TTL` |
| 6 | `BatchGet` | Yes | `BGET` |
| 7 | `BatchPut` | Yes | `BPUT` |
| 8 | `BatchDelete` | Yes | `BDELETE` |
| 9 | `Scan` | Yes | `SCAN` |
| 10 | `DeleteRange` | Yes | `DELETE RANGE` |
| 11 | `CompareAndSwap` | Yes | `CAS` |
| 12 | `Checksum` | Yes | `CHECKSUM` |

All 12 RawKVClient methods are covered. (`Close` is a no-op lifecycle method, not a user-facing command.)

### TxnHandle Methods (6 public methods)

| # | Method | Covered? | Command |
|---|--------|----------|---------|
| 1 | `Get` | Yes | `GET` (in txn) / `TGET` |
| 2 | `BatchGet` | Yes | `BGET` (in txn) / `TBGET` |
| 3 | `Set` | Yes | `SET` (in txn) / `TSET` |
| 4 | `Delete` | Yes | `DELETE` (in txn) / `TDEL` |
| 5 | `Commit` | Yes | `COMMIT` |
| 6 | `Rollback` | Yes | `ROLLBACK` |

All 6 public TxnHandle methods are covered. (`StartTS` is exposed implicitly in the BEGIN output.)

Note: `TxnHandle` has no `Scan` method, so the absence of a transactional SCAN command is correct.

### TxnKVClient Methods

| # | Method | Covered? | Command |
|---|--------|----------|---------|
| 1 | `Begin` | Yes | `BEGIN` |
| 2 | `Close` | N/A | Lifecycle, not user-facing |

### gookv-ctl Online Commands

| # | Command | Covered? | CLI Command |
|---|---------|----------|-------------|
| 1 | `store list` | Yes | `STORE LIST` |
| 2 | `store status` | Yes | `STORE STATUS <id>` |

Both existing online commands are covered.

### pdclient.Client Methods -- Coverage Analysis

| # | Method | Covered? | Notes |
|---|--------|----------|-------|
| 1 | `GetTS` | Yes | `TSO` command |
| 2 | `GetRegion` | Yes | `REGION <key>` |
| 3 | `GetRegionByID` | Yes | `REGION ID <id>` (in `01_overview.md`) |
| 4 | `GetStore` | Yes | `STORE STATUS <id>` |
| 5 | `GetAllStores` | Yes | `STORE LIST` |
| 6 | `GetGCSafePoint` | Yes | `GC SAFEPOINT` |
| 7 | `GetClusterID` | Yes | `CLUSTER INFO` |
| 8 | `UpdateGCSafePoint` | No | See [C-1] |
| 9 | `AllocID` | No | Internal, probably fine to omit |
| 10 | `Bootstrap` | No | Internal, correct to omit |
| 11 | `IsBootstrapped` | No | See [C-2] |
| 12 | `PutStore` | No | Internal, correct to omit |
| 13 | `ReportRegionHeartbeat` | No | Internal, correct to omit |
| 14 | `StoreHeartbeat` | No | Internal, correct to omit |
| 15 | `AskBatchSplit` | No | Internal, correct to omit |
| 16 | `ReportBatchSplit` | No | Internal, correct to omit |

**[C-1] Missing: UPDATE GC SAFEPOINT command (Low)**
`UpdateGCSafePoint` is useful for debugging GC issues. Consider adding `GC SAFEPOINT SET <ts>` as an admin command. Low priority since it is a mutating operation that could be dangerous.

**[C-2] Missing: Cluster bootstrap check (Low)**
`IsBootstrapped` would be useful as part of `CLUSTER INFO` output (e.g., showing "Bootstrapped: yes"). Not critical -- the cluster must already be bootstrapped for the CLI to connect.

**[C-3] REGION ID command present in 01_overview.md but missing from 02_command_reference.md (Medium)**
`01_overview.md` Section 6.3 lists `REGION ID <id>` mapping to `pdclient.GetRegionByID`. However, `02_command_reference.md` does not have a section for `REGION ID`. It only documents `REGION <key>` and `REGION LIST`. Add a section for `REGION ID <id>` in the command reference.

**[C-4] REGION LIST and TSO and STATUS present in 02 but missing from 01_overview.md (Medium)**
`02_command_reference.md` defines `REGION LIST`, `TSO`, and `STATUS` commands that are not listed in `01_overview.md` Section 6.3. The overview table should be the authoritative quick reference and needs to include all commands.

---

## 3. Consistency

**[S-1] BEGIN option naming mismatch between documents (Medium)**
- `01_overview.md`: `BEGIN [PESSIMISTIC] [ASYNC] [1PC] [LOCKTTL <n>]`
- `02_command_reference.md` EBNF: `BEGIN [PESSIMISTIC] [ASYNC_COMMIT] [ONE_PC] [LOCK_TTL <ms>]`

The option names should be consistent. `02_command_reference.md` uses names that more closely match the Go function names (`WithAsyncCommit`, `With1PC`, `WithLockTTL`). Recommendation: use the `02_command_reference.md` forms (`ASYNC_COMMIT`, `ONE_PC`, `LOCK_TTL`) as the canonical names, and update `01_overview.md` accordingly. `1PC` vs `ONE_PC` is particularly confusing -- `ONE_PC` is unambiguous.

**[S-2] Output format inconsistency: tables in 01 vs quoted strings in 02 (Medium)**
- `01_overview.md` shows GET results in a table format with borders:
  ```
  +-----------+
  | value     |
  +-----------+
  | myvalue   |
  +-----------+
  ```
- `02_command_reference.md` shows GET results as a quoted string:
  ```
  "hello world"
  (1 row, 0.8ms)
  ```

These are fundamentally different output styles. The table format is consistent with SCAN/BGET output but verbose for single values. The quoted-string format is compact but inconsistent with multi-row output. Pick one. The table format from `01_overview.md` is more consistent internally, but the quoted format from `02_command_reference.md` is more ergonomic for single values. A reasonable compromise: use tables for multi-row results (SCAN, BGET, STORE LIST) and quoted strings for single-value results (GET, TGET).

**[S-3] Timing format inconsistency (Minor)**
- `01_overview.md`: `(0.3 ms)` -- space before `ms`
- `02_command_reference.md`: `(0.8ms)` -- no space before `ms`

Pick one. No space (`0.8ms`) is more compact and common in CLI tools.

**[S-4] "not found" display inconsistency (Minor)**
- `01_overview.md`: `(not found)`
- `02_command_reference.md` (raw GET): `(not found)` with `(0 rows, ...)`
- `02_command_reference.md` (txn GET): `(nil)`

Inside a transaction, TiKV's convention is `(nil)` (like redis-cli). Outside, `(not found)` is more descriptive. This is a deliberate context-dependent choice, which is fine, but document the rationale explicitly.

**[S-5] Meta command syntax divergence between documents (Medium)**
- `01_overview.md`: backslash meta commands (`\h`, `\q`, `\x`, `\t`) that don't require semicolons
- `02_command_reference.md`: keyword meta commands (`HELP;`, `EXIT;`, `QUIT;`, `SET TIMING ON;`, `SET FORMAT TABLE;`) that require semicolons

Both approaches are valid (psql supports both). The documents should clarify that both forms work: `\h` and `HELP;` are equivalent, `\q` and `EXIT;` are equivalent, etc. Currently `01_overview.md` lists only backslash forms and `02_command_reference.md` lists only keyword forms, making it look like two different designs.

Also, the `\x` toggle from `01_overview.md` (cycle through auto/hex/string) is replaced by `SET FORMAT TABLE|PLAIN|HEX` in `02_command_reference.md`. These have different semantics: `\x` cycles display encoding; `SET FORMAT` changes output structure (table vs. plain vs. hex). Clarify whether both exist or one replaces the other.

---

## 4. Missing Features

**[M-1] No pipe input from file / stdin redirect (Medium)**
Users expect `gookv-cli --pd ... < commands.txt` to work. The batch mode (`-c`) handles single invocations, but piping a file of commands through stdin is a standard pattern (psql, mysql, redis-cli all support it). The REPL should detect when stdin is not a TTY and switch to a non-interactive mode automatically (no prompts, no readline, execute line-by-line).

**[M-2] No output redirection / file output flag (Low)**
Something like `--output file.txt` or the ability to use `\o filename` (psql-style) to redirect output to a file. Not critical for v1, but worth noting as a future enhancement.

**[M-3] No tab completion (Low)**
Tab completion for command names (GET, PUT, SCAN, etc.) and sub-commands (STORE LIST, STORE STATUS) would be a significant UX win. `chzyer/readline` supports custom completers. The implementation plan should mention this as a Phase 2 enhancement at minimum.

**[M-4] No connection string / multi-PD-endpoint format documentation (Low)**
The `--pd` flag accepts comma-separated endpoints but this isn't documented in the spec. Example: `--pd "10.0.0.1:2379,10.0.0.2:2379,10.0.0.3:2379"`. The implementation sketch in `03_implementation_plan.md` references `splitEndpoints(pd)` but the user-facing documentation should show the multi-endpoint format.

**[M-5] No auth/TLS support (Low)**
No mention of TLS certificates or authentication. This is fine for an initial version of a dev tool, but should be explicitly called out as a future enhancement if gookv adds TLS to its gRPC connections.

**[M-6] No `--version` flag (Minor)**
Standard CLI practice. Easy to add.

**[M-7] No WATCH or SUBSCRIBE mechanism (Minor)**
Redis-cli has `MONITOR` and `SUBSCRIBE`. Not essential for a KV store CLI, but a `WATCH <key>` that polls and prints changes would be interesting for debugging. Low priority.

---

## 5. Implementation Concerns

**[I-1] Client struct does not expose pdClient (Blocker for implementation)**
The design doc's `Executor` struct holds `pdClient pdclient.Client` as a separate field. However, `pkg/client.Client` has `pdClient` as an unexported field with no public accessor. Implementation will require either:
- Adding a `func (c *Client) PD() pdclient.Client` method to `pkg/client/client.go`, or
- Having the CLI create its own `pdclient.Client` directly (duplicating the PD connection).

The first option is cleaner. This should be noted in the implementation plan.

**[I-2] Executor creates rawkv and txnkv eagerly but only one is used at a time (Minor)**
The design creates both `RawKVClient` and `TxnKVClient` at startup. Since `RawKV()` and `TxnKV()` are cheap (just struct construction), this is fine and not a real problem.

**[I-3] Error handling on commit failure needs clarification (Low)**
`02_command_reference.md` Section 3.5 says: "On commit failure (except 'already committed'), the transaction remains active." But `03_implementation_plan.md` Section 4 (Step 4) says: `e.activeTxn = nil` is set unconditionally after `Commit`. The implementation should match the spec: only nil out `activeTxn` on success or "already committed" error, keeping the handle alive on other failures so the user can ROLLBACK.

---

## 6. Summary

The design is well-structured and thorough. The main issues to address before implementation:

**Must fix:**
1. [U-1] Resolve the TGET/TSET/TDEL vs context-sensitive GET/SET/DELETE contradiction between documents
2. [C-3] Add REGION ID to `02_command_reference.md`
3. [C-4] Add REGION LIST, TSO, STATUS to `01_overview.md` Section 6.3
4. [I-1] Plan for exposing `pdclient.Client` from `pkg/client.Client`

**Should fix:**
5. [S-1] Unify BEGIN option names across documents
6. [S-2] Decide on single-value output format (table vs quoted string)
7. [S-5] Clarify that both backslash and keyword meta commands are supported
8. [U-2] Settle SCAN limit syntax (positional vs LIMIT keyword)
9. [U-3] Settle BDEL vs BDELETE
10. [U-4] Settle DELRANGE vs DELETE RANGE
11. [I-3] Align commit failure handling between spec and implementation

**Nice to have for v1:**
12. [M-1] Auto-detect non-TTY stdin for pipe/file input
13. [M-4] Document multi-endpoint PD connection format
14. [M-6] Add --version flag

**Future enhancements:**
15. [M-3] Tab completion
16. [M-2] Output to file
17. [M-5] TLS/auth support
18. [C-1] GC SAFEPOINT SET command

---

## Second Review (2026-03-28)

Reviewer: Claude Opus 4.6 (source-verified review)
Focus: Implementability, EBNF grammar accuracy, Go type accuracy, edge cases, dependency feasibility

### 1. Implementability Assessment

The spec is **implementable** by a coding agent with moderate guidance, but several
ambiguities and contradictions between documents would cause implementation stalls
without human clarification. The first review already identified the major
cross-document contradictions ([U-1] through [U-4], [S-1] through [S-5]). Below are
additional implementability gaps not covered by the first review.

**[Impl-1] SET is ambiguous as both a meta command and a txn command (Blocker)**
The EBNF in `02_command_reference.md` defines:
- `txn_set_cmd = "SET" key value ;` (transactional write)
- `set_timing_cmd = "SET" "TIMING" ( "ON" | "OFF" ) ;` (meta command)
- `set_format_cmd = "SET" "FORMAT" ( "TABLE" | "PLAIN" | "HEX" ) ;` (meta command)

A parser receiving `SET TIMING ON;` cannot determine from the grammar alone whether
this is `txn_set_cmd` with key=`TIMING` and value=`ON`, or `set_timing_cmd`. Inside
a transaction, this ambiguity becomes a real parsing conflict. The parser must peek
ahead to check whether the second token is `TIMING` or `FORMAT` before deciding. This
is implementable but the EBNF as written is ambiguous -- the grammar needs explicit
disambiguation (e.g., `set_timing_cmd` takes priority over `txn_set_cmd` when the
second token is `TIMING` or `FORMAT`, or require a different keyword for txn writes).

**Recommendation:** Either (a) add a parser note that `SET TIMING` / `SET FORMAT` are
always meta commands regardless of transaction state, or (b) rename transactional
write to `PUT` inside transactions as well (consistent with the T-prefixed approach
in `01_overview.md` where it's `TSET`).

**[Impl-2] No specification of how Executor obtains pdclient.Client (Medium)**
The first review noted [I-1] that `pdClient` is unexported on `Client`. Beyond that,
the `03_implementation_plan.md` Step 1 shows `NewExecutor(c)` taking only a
`*client.Client`, but the `Executor` struct has a `pdClient pdclient.Client` field.
The implementation plan needs to specify how to bridge this gap. The cleanest approach:
add a `func (c *Client) PD() pdclient.Client` accessor to `pkg/client/client.go`.

**[Impl-3] TxnHandle.Get "not found" semantics differ from RawKVClient.Get (Medium)**
Verified from source:
- `RawKVClient.Get(ctx, key)` returns `([]byte, bool, error)` -- the `bool` is an
  explicit "notFound" indicator.
- `TxnHandle.Get(ctx, key)` returns `([]byte, error)` -- a nil `[]byte` with nil
  error means "not found" (or key was deleted in buffer).

The spec's executor sketch in `03_implementation_plan.md` Step 4 shows:
```go
val, notFound, err := e.rawkv.Get(ctx, cmd.Args[0])
```
This is correct for raw. But for transactional GET, the executor must check
`val == nil && err == nil` to detect not-found. This distinction is not documented
anywhere in the spec and a coding agent may not discover it without reading the
TxnHandle source.

**Recommendation:** Add a note in the executor section of `03_implementation_plan.md`
explaining how to detect not-found for both raw and transactional GET.

**[Impl-4] StartTS type mismatch between spec and source (Low)**
The `02_command_reference.md` BEGIN output shows `startTS=443552187662221312` as a
plain uint64. In the actual code, `TxnHandle.StartTS()` returns
`txntypes.TimeStamp` (which is `type TimeStamp uint64`). This is compatible, but
for the TSO command output that decomposes into physical/logical components, the
implementer will need to use `pdclient.TimeStampFromUint64()` or manually shift
bits (`physical = ts >> 18`, `logical = ts & ((1<<18)-1)`). Neither conversion
function is mentioned in the spec.

### 2. EBNF Grammar Accuracy

I tested the EBNF grammar in `02_command_reference.md` Section 1 against every
command example in the document. Findings:

**[G-1] `batch_delete_cmd` uses `BDELETE` but `01_overview.md` uses `BDEL` (Already noted)**
Already covered by [U-3]. The EBNF uses `BDELETE`.

**[G-2] `delete_range_cmd` starts with `DELETE RANGE` but shares the `DELETE` keyword (Medium)**
The EBNF defines:
```
raw_cmd = ... | delete_cmd | ... | delete_range_cmd | ...
delete_cmd = "DELETE" key ;
delete_range_cmd = "DELETE" "RANGE" key key ;
```
A parser seeing `DELETE RANGE ...` must disambiguate between `delete_cmd` with
key=`RANGE` and `delete_range_cmd`. This is resolvable by checking whether the second
token is the keyword `RANGE` (case-insensitive). The EBNF is technically ambiguous but
practically parseable with 1-token lookahead. Worth noting in parsing rules.

**[G-3] SCAN example uses positional limit, EBNF requires LIMIT keyword**
`01_overview.md`: `SCAN k1 k3 10` (bare integer)
`02_command_reference.md` EBNF: `scan_cmd = "SCAN" key key [ "LIMIT" integer ]`
`02_command_reference.md` example: `SCAN "user:" "user:~" LIMIT 5`

The EBNF requires the `LIMIT` keyword. The `01_overview.md` example `SCAN k1 k3 10`
does NOT parse under the EBNF because `10` would be interpreted as a third key, not
a limit. Already partially noted by [U-2], but the grammar breakage is worth
emphasizing: the EBNF and `01_overview.md` are incompatible, not just inconsistent.

**[G-4] `checksum_cmd` optional range uses `[ key key ]` -- ambiguous with no-arg form**
The EBNF: `checksum_cmd = "CHECKSUM" [ key key ] ;`
With one argument, e.g., `CHECKSUM "prefix:"`, the parser cannot tell if this is
an incomplete two-key range or an error. The grammar requires either 0 or 2 arguments.
This is actually correct and clear in the EBNF, but the parser must validate argument
count (0 or 2) and reject 1.

**[G-5] CAS `NOT_EXIST` flag not fully represented in EBNF**
The EBNF: `cas_cmd = "CAS" key value value [ "NOT_EXIST" ] ;`
But Section 2.10 says: `CAS <key> <new_value> _ NOT_EXIST` where `_` is a placeholder
for the expected_old argument. The EBNF parses this as key=`<key>`, value1=`<new>`,
value2=`_`, optional flag=`NOT_EXIST`. The `_` is just a regular unquoted_token
being used as a convention. This works but is fragile -- what if someone writes
`CAS k1 v1 NOT_EXIST` without the `_` placeholder? The parser would interpret
`NOT_EXIST` as the expected_old value, which is silently wrong. The grammar should
clarify that `NOT_EXIST` as a bare fourth token is always the flag, not a value.

**[G-6] `region_list_cmd` defined in EBNF but `REGION ID` is not**
The EBNF defines `region_list_cmd = "REGION" "LIST" [ "LIMIT" integer ]` but
does not define a production for `REGION ID <id>` which is listed in
`01_overview.md` Section 6.3. Already noted by [C-3].

**[G-7] `status_cmd` uses `string` token which overlaps with `key`**
`status_cmd = "STATUS" [ string ] ;` where `string = quoted_string | unquoted_token`.
Since `key` is also `quoted_string | hex_literal | unquoted_token`, `string` is a
subset of `key`. This is fine but inconsistent naming -- consider using `key` for
both, or define a `token` non-terminal that both reference.

**[G-8] Meta commands in EBNF require semicolons but 01_overview.md says they don't**
The EBNF top-level: `statement = command ";" ;` and `command = ... | meta_cmd ;`.
This means `HELP;` requires a semicolon. But `01_overview.md` Section 6.4 says
meta commands prefixed with `\` do not require semicolons. The EBNF does not
define the backslash meta commands (`\h`, `\q`, `\x`, `\t`) at all. They should
either be added to the grammar or explicitly noted as outside the formal grammar
(handled by the REPL layer before parsing).

### 3. Go Type Accuracy

Verified every type reference in `03_implementation_plan.md` against actual source
code using Serena LSP.

| Spec Reference | Actual Source | Match? | Notes |
|---|---|---|---|
| `client.Client` | `pkg/client.Client` struct | Yes | |
| `client.NewClient(ctx, Config{PDAddrs: ...})` | `NewClient(ctx context.Context, cfg Config)` | Yes | `Config.PDAddrs` is `[]string` |
| `client.RawKVClient` | `pkg/client.RawKVClient` struct | Yes | |
| `client.TxnKVClient` | `pkg/client.TxnKVClient` struct | Yes | |
| `client.TxnHandle` | `pkg/client.TxnHandle` struct | Yes | |
| `client.TxnOption` | `type TxnOption func(*TxnOptions)` | Yes | |
| `client.KvPair` | `struct { Key, Value []byte }` | Yes | |
| `pdclient.Client` | Interface in `pkg/pdclient` | Yes | |
| `RawKVClient.Get(ctx, key) ([]byte, bool, error)` | Actual signature matches | Yes | |
| `RawKVClient.Put(ctx, key, value) error` | Actual signature matches | Yes | |
| `RawKVClient.PutWithTTL(ctx, key, value, ttl) error` | `ttl` is `uint64` | Yes | |
| `RawKVClient.Delete(ctx, key) error` | Actual signature matches | Yes | |
| `RawKVClient.GetKeyTTL(ctx, key) (uint64, error)` | Actual signature matches | Yes | |
| `RawKVClient.Scan(ctx, start, end, limit) ([]KvPair, error)` | `limit` is `int` | Yes | |
| `RawKVClient.BatchGet(ctx, keys) ([]KvPair, error)` | `keys` is `[][]byte` | Yes | |
| `RawKVClient.BatchPut(ctx, pairs) error` | `pairs` is `[]KvPair` | Yes | |
| `RawKVClient.BatchDelete(ctx, keys) error` | `keys` is `[][]byte` | Yes | |
| `RawKVClient.DeleteRange(ctx, start, end) error` | Actual signature matches | Yes | |
| `RawKVClient.CompareAndSwap(ctx, key, value, prevValue, prevNotExist) (bool, []byte, error)` | Actual signature matches | Yes | 5 params confirmed |
| `RawKVClient.Checksum(ctx, start, end) (uint64, uint64, uint64, error)` | Actual signature matches | Yes | Returns (checksum, kvs, bytes, err) |
| `TxnKVClient.Begin(ctx, opts ...TxnOption) (*TxnHandle, error)` | Actual signature matches | Yes | |
| `TxnHandle.Get(ctx, key) ([]byte, error)` | Actual signature matches | Yes | No bool for not-found |
| `TxnHandle.BatchGet(ctx, keys) ([]KvPair, error)` | Actual signature matches | Yes | |
| `TxnHandle.Set(ctx, key, value) error` | Actual signature matches | Yes | |
| `TxnHandle.Delete(ctx, key) error` | Actual signature matches | Yes | |
| `TxnHandle.Commit(ctx) error` | Actual signature matches | Yes | |
| `TxnHandle.Rollback(ctx) error` | Actual signature matches | Yes | |
| `WithPessimistic() TxnOption` | Actual signature matches | Yes | |
| `WithAsyncCommit() TxnOption` | Actual signature matches | Yes | |
| `With1PC() TxnOption` | Actual signature matches | Yes | |
| `WithLockTTL(ttl uint64) TxnOption` | Actual signature matches | Yes | |
| `pdclient.Client.GetAllStores(ctx) ([]*metapb.Store, error)` | Actual signature matches | Yes | |
| `pdclient.Client.GetStore(ctx, storeID) (*metapb.Store, error)` | `storeID` is `uint64` | Yes | |
| `pdclient.Client.GetRegion(ctx, key) (*metapb.Region, *metapb.Peer, error)` | Actual signature matches | Yes | |
| `pdclient.Client.GetRegionByID(ctx, regionID) (*metapb.Region, *metapb.Peer, error)` | `regionID` is `uint64` | Yes | |
| `pdclient.Client.GetTS(ctx) (TimeStamp, error)` | Returns `pdclient.TimeStamp` struct | Yes | Struct with Physical/Logical fields |
| `pdclient.Client.GetGCSafePoint(ctx) (uint64, error)` | Actual signature matches | Yes | |
| `pdclient.Client.GetClusterID(ctx) uint64` | Returns `uint64` (no error) | Yes | |
| Spec: `c.Close()` | `(*Client).Close() error` | **Mismatch** | Returns error, spec ignores it |

**[T-1] `Client.Close()` returns `error` but spec ignores it (Minor)**
`03_implementation_plan.md` uses `defer c.Close()` which discards the error. The
actual implementation always returns nil, so this is safe in practice. But good Go
style would be `defer func() { _ = c.Close() }()` or at least a comment explaining
why the error is intentionally ignored.

**[T-2] `TxnHandle.StartTS()` returns `txntypes.TimeStamp` not `uint64` (Low)**
The spec's BEGIN output shows `startTS=443552187662221312` implying a plain uint64.
The actual return type is `txntypes.TimeStamp` which is `type TimeStamp uint64`, so
`uint64(txn.StartTS())` works fine. But the TSO command output requires decomposition
into physical/logical, which needs either `pdclient.TimeStampFromUint64()` or manual
bit manipulation. The spec should mention this conversion.

**[T-3] Executor struct references non-existent accessor pattern (Medium)**
The spec's `Executor` struct has `pdClient pdclient.Client` but `pkg/client.Client`
has no public method returning its `pdClient`. As the first review noted in [I-1],
a `PD()` accessor must be added. Alternatively, the CLI's `main.go` could create
a separate `pdclient.NewClient()` with the same endpoints, but this duplicates the
gRPC connection.

### 4. Edge Cases

**[E-1] Empty keys: Partially addressed**
Section 6.4 of `02_command_reference.md` says `""` as a key for GET/PUT/DELETE is
rejected with `ERROR: key must not be empty`. For SCAN/DELETE RANGE, `""` means
start/end of keyspace. This is correct and well-documented. However, the parser
section (`03_implementation_plan.md` Step 3) does not mention validation of empty
keys. The `ParseCommand` function should not reject empty keys -- that validation
belongs in the executor, since `""` is valid for range boundaries.

**[E-2] Binary values: Well addressed**
The hex literal syntax (`0x...` and `x'...'`) handles binary input. Output encoding
is controlled by display mode. No gaps here.

**[E-3] Very long scan results: Not addressed (Medium)**
If `SCAN a z LIMIT 10000` returns 10,000 rows, the entire result is rendered to
stdout at once. There is no pagination mechanism. For REPL mode, consider:
- A default display limit (e.g., show first 100 rows, then `-- More (9900 remaining) --`)
- A `\pager` command to pipe through `less` (psql-style)
- At minimum, document that large scans will flood the terminal.

For batch mode, streaming all results is correct. But for interactive use, this is
a UX problem.

**[E-4] Connection failure mid-command: Partially addressed**
The error display section shows examples of `ERROR: rpc error: <detail>` and
`ERROR: region error: <detail>`. However, the spec does not describe what happens
when the PD connection is lost mid-session. Questions:
- Does the REPL keep running and retry on the next command? (It should, since
  `pkg/client` has retry logic internally.)
- Should there be a `\reconnect` command?
- What if PD is unreachable at startup? (This is handled: the spec shows
  `os.Exit(1)` on connection failure.)

**[E-5] Ctrl-C during long operation: Partially addressed (Medium)**
The spec says Ctrl-C in Idle state is a no-op and in Collecting state discards the
buffer. But what about Ctrl-C during a long-running SCAN or COMMIT that takes
seconds? The `signal.NotifyContext` in `main.go` creates a cancellable context, but
if the REPL passes this context to `exec.Exec()`, a Ctrl-C during execution would
cancel the context and terminate the entire CLI (since `signal.NotifyContext`
cancels on first signal). The REPL should instead use a per-command child context
derived from the main context, so that Ctrl-C during execution cancels only the
current operation, not the session.

**Recommendation:** In the REPL loop, create a per-command context:
```go
cmdCtx, cmdCancel := context.WithCancel(ctx)
// Wire readline interrupt to cmdCancel
result, err := exec.Exec(cmdCtx, cmd)
cmdCancel()
```
This way, Ctrl-C cancels only the in-flight RPC while the REPL continues. The spec
should document this pattern.

**[E-6] Transaction timeout: Not addressed (Medium)**
What happens if a user does `BEGIN;` and then walks away for 30 minutes? The
transaction's locks have a TTL (default 3000ms per `WithLockTTL`). After the TTL
expires, other transactions can resolve the locks. When the user returns and types
`COMMIT;`, the prewrite will fail because locks were cleaned up.

The spec should document:
- What the expected error message is when committing a timed-out transaction.
- Whether the REPL should proactively warn when the lock TTL is about to expire
  (probably not for v1, but worth noting).
- That `LOCK_TTL` should be set higher for interactive use (e.g., 60000ms) since
  humans are slower than automated clients.

**[E-7] Concurrent raw KV and transaction operations: Addressed**
The spec correctly blocks raw KV commands inside a transaction and suggests using
T-prefixed / context-sensitive variants. No gap here.

**[E-8] Extremely long keys/values: Not addressed (Low)**
gookv may have limits on key/value size (TiKV defaults to 6MB for values, 10KB for
keys). The CLI should propagate server-side errors for oversized keys/values, which
it does implicitly via error returns. No spec change needed, but a note about size
limits in the HELP output would be user-friendly.

### 5. Dependency Feasibility

**github.com/chzyer/readline v1.5.1**
- Pure Go, no CGo. Compatible with any Go version >= 1.13.
- Go 1.25 (as specified in `go.mod`) is well above the minimum.
- Used by CockroachDB's `cockroach sql` and TiDB's `tiup`. Well-maintained.
- Provides exactly the features needed: custom prompt, history file, Ctrl-C/Ctrl-D
  handling, interrupt as `ErrInterrupt`.
- **Verdict: Compatible, no issues.**

**github.com/olekukonko/tablewriter v0.0.5**
- Pure Go, no CGo. Compatible with Go >= 1.12.
- Go 1.25 is well above the minimum.
- Widely used (70M+ downloads). API is stable at v0.0.5.
- Note: v0.0.5 is the latest in the v0 line. There is a v2 rewrite
  (`github.com/olekukonko/tablewriter/v2`) but v0.0.5 is still maintained and
  sufficient. The spec should pin the version explicitly.
- **Verdict: Compatible, no issues.**

Both libraries have no CGo dependencies, which is consistent with gookv's existing
pure-Go dependency strategy (no CGo in `go.mod`).

### 6. Additional Findings

**[A-1] Existing MockClient in pkg/pdclient/mock.go is ready for testing (Info)**
The test plan in `03_implementation_plan.md` Section 7.2 mentions "mock RawKVClient
and TxnKVClient (or pdclient.MockClient)." The codebase already has
`pkg/pdclient.MockClient` with `NewMockClient()` implementing the full
`pdclient.Client` interface. There is no existing mock for `RawKVClient` or
`TxnKVClient`, so the test plan will need to either create interface wrappers for
these or use integration tests with a real cluster.

**[A-2] Makefile integration is straightforward (Info)**
The spec's proposed Makefile addition (`gookv-cli` target added to `build:`) is
consistent with the existing Makefile structure. The current `build:` target is
`build: gookv-server gookv-ctl gookv-pd`. Adding `gookv-cli` is mechanical. No
`clean:` target currently exists, so adding one would be net new.

**[A-3] `pdclient.Client.Close()` returns nothing but `client.Client.Close()` returns error (Info)**
The `pdclient.Client` interface's `Close()` method has no return value. The
`client.Client.Close()` method returns `error` but always returns nil. This
inconsistency within the codebase is not the spec's problem, but implementers
should be aware that `defer c.Close()` (on `client.Client`) will trigger a
"return value not checked" linter warning.

### 7. Verdict

**Overall: Ready for implementation with caveats.**

The spec is detailed, well-organized, and covers the vast majority of what a coding
agent needs. The Go type mappings are accurate (verified against source). The main
blockers are:

1. **[Impl-1]** The `SET` keyword collision between txn write and meta commands is a
   real parsing ambiguity that must be resolved before implementation.
2. **[I-1]/[Impl-2]/[T-3]** The `pdClient` accessor gap is a code-level blocker --
   a one-line fix to `pkg/client/client.go` but must be done first.
3. **[E-5]** The `signal.NotifyContext` pattern will cause Ctrl-C to kill the entire
   CLI during long operations. Needs a per-command context pattern.
4. Cross-document contradictions ([U-1], [S-1], [S-2], [S-5]) must be resolved to
   avoid the coding agent making arbitrary choices.

Everything else is implementable as specified.
