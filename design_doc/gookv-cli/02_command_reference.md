# gookv-cli Command Reference

This document defines the complete command language for `gookv-cli`, the interactive REPL client for gookv. Every command maps directly to a method on `RawKVClient`, `TxnKVClient`/`TxnHandle`, or `pdclient.Client`.

---

## 1. Grammar (EBNF)

```ebnf
(* Top-level *)
statement       = command ";" ;
command         = raw_cmd | txn_cmd | admin_cmd | meta_cmd ;

(* Raw KV commands *)
raw_cmd         = get_cmd | put_cmd | delete_cmd | scan_cmd
                | batch_get_cmd | batch_put_cmd | batch_delete_cmd
                | delete_range_cmd | ttl_cmd | cas_cmd | checksum_cmd ;

get_cmd         = "GET" key ;
put_cmd         = "PUT" key value [ "TTL" integer ] ;
delete_cmd      = "DELETE" key ;
delete_range_cmd = "DELETE" "RANGE" key key ;
scan_cmd        = "SCAN" key key [ "LIMIT" integer ] ;
batch_get_cmd   = "BGET" key { key } ;
batch_put_cmd   = "BPUT" key value { key value } ;
batch_delete_cmd = "BDELETE" key { key } ;
ttl_cmd         = "TTL" key ;
cas_cmd         = "CAS" key value value [ "NOT_EXIST" ] ;
checksum_cmd    = "CHECKSUM" [ key key ] ;

(* Transactional commands *)
txn_cmd         = begin_cmd | txn_set_cmd | txn_get_cmd
                | txn_delete_cmd | commit_cmd | rollback_cmd ;

begin_cmd       = "BEGIN" { begin_opt } ;
begin_opt       = "PESSIMISTIC" | "ASYNC_COMMIT" | "ONE_PC"
                | "LOCK_TTL" integer ;
txn_set_cmd     = "SET" key value ;
txn_get_cmd     = "GET" key ;
txn_delete_cmd  = "DELETE" key ;
commit_cmd      = "COMMIT" ;
rollback_cmd    = "ROLLBACK" ;

(* Administrative commands *)
admin_cmd       = store_list_cmd | store_status_cmd
                | region_cmd | region_list_cmd
                | cluster_info_cmd | tso_cmd
                | gc_safepoint_cmd | status_cmd ;

store_list_cmd  = "STORE" "LIST" ;
store_status_cmd = "STORE" "STATUS" integer ;
region_cmd      = "REGION" key ;
region_list_cmd = "REGION" "LIST" [ "LIMIT" integer ] ;
cluster_info_cmd = "CLUSTER" "INFO" ;
tso_cmd         = "TSO" ;
gc_safepoint_cmd = "GC" "SAFEPOINT" ;
status_cmd      = "STATUS" [ string ] ;

(* Meta commands *)
meta_cmd        = help_cmd | exit_cmd | set_timing_cmd | set_format_cmd ;

help_cmd        = "HELP" ;
exit_cmd        = "EXIT" | "QUIT" ;
set_timing_cmd  = "SET" "TIMING" ( "ON" | "OFF" ) ;
set_format_cmd  = "SET" "FORMAT" ( "TABLE" | "PLAIN" | "HEX" ) ;

(* Tokens *)
key             = quoted_string | hex_literal | unquoted_token ;
value           = quoted_string | hex_literal | unquoted_token ;
string          = quoted_string | unquoted_token ;
integer         = digit { digit } ;
digit           = "0" | "1" | "2" | "3" | "4" | "5" | "6" | "7" | "8" | "9" ;

quoted_string   = '"' { any_char | escape_seq } '"' ;
escape_seq      = "\\" | '\"' | "\n" | "\t" ;
hex_literal     = ( "x'" hex_digits "'" ) | ( "0x" hex_digits ) ;
hex_digits      = hex_digit { hex_digit } ;
hex_digit       = digit | "a" | "b" | "c" | "d" | "e" | "f"
                        | "A" | "B" | "C" | "D" | "E" | "F" ;
unquoted_token  = visible_char { visible_char } ;
(* unquoted_token may not contain whitespace, semicolons, or double-quotes *)
```

**Parsing rules:**

- Commands are case-insensitive (`get`, `GET`, `Get` are all valid).
- Trailing `;` is required. The REPL may accept bare Enter as an implicit `;` for single-line commands.
- When a transaction is active, `GET` and `DELETE` dispatch to the transactional variants (`TxnHandle.Get`, `TxnHandle.Delete`). Outside a transaction they dispatch to `RawKVClient.Get` and `RawKVClient.Delete`.
- `SET` is only valid inside a transaction.

---

## 2. Raw KV Commands

### 2.1 GET

Retrieve a single key's value.

**Syntax:**
```
GET <key>;
```

**Maps to:** `RawKVClient.Get(ctx, key) ([]byte, bool, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to look up |

**Output:**
```
gookv> GET mykey;
"hello world"
(1 row, 0.8ms)

gookv> GET nonexistent;
(not found)
(0 rows, 0.5ms)
```

**Errors:**
- Region not reachable: `ERROR: region error: <detail>`
- Connection failure: `ERROR: rpc error: <detail>`

---

### 2.2 PUT

Store a key-value pair, optionally with a TTL.

**Syntax:**
```
PUT <key> <value>;
PUT <key> <value> TTL <seconds>;
```

**Maps to:**
- Without TTL: `RawKVClient.Put(ctx, key, value) error`
- With TTL: `RawKVClient.PutWithTTL(ctx, key, value, ttl) error` where `ttl` is `uint64` seconds.

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to write |
| `value` | value | The value to store |
| `seconds` | integer | (Optional) TTL in seconds; 0 means no expiration |

**Output:**
```
gookv> PUT greeting "hello world";
OK
(0.9ms)

gookv> PUT session token123 TTL 3600;
OK
(1.1ms)
```

**Errors:**
- Empty key: `ERROR: key must not be empty`
- Region error: `ERROR: region error: <detail>`

---

### 2.3 DELETE

Remove a single key.

**Syntax:**
```
DELETE <key>;
```

**Maps to:** `RawKVClient.Delete(ctx, key) error`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to remove |

**Output:**
```
gookv> DELETE mykey;
OK
(0.7ms)
```

Deleting a nonexistent key is not an error -- it succeeds silently.

---

### 2.4 TTL

Query the remaining TTL for a key.

**Syntax:**
```
TTL <key>;
```

**Maps to:** `RawKVClient.GetKeyTTL(ctx, key) (uint64, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to query |

**Output:**
```
gookv> TTL session;
TTL: 3542s
(0.6ms)

gookv> TTL no_ttl_key;
TTL: 0 (no expiration)
(0.5ms)
```

---

### 2.5 SCAN

Retrieve key-value pairs in a lexicographic range. Transparently crosses region boundaries.

**Syntax:**
```
SCAN <start_key> <end_key> [LIMIT <n>];
```

**Maps to:** `RawKVClient.Scan(ctx, startKey, endKey, limit) ([]KvPair, error)` where `limit` is `int`. Default limit: 20.

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `start_key` | key | Inclusive start of range. Use `""` for beginning of keyspace |
| `end_key` | key | Exclusive end of range. Use `""` for end of keyspace |
| `n` | integer | (Optional) Maximum rows to return. Default: 20 |

**Output:**
```
gookv> SCAN "user:" "user:~" LIMIT 5;
+----------+--------+
| Key      | Value  |
+----------+--------+
| user:001 | alice  |
| user:002 | bob    |
| user:003 | carol  |
+----------+--------+
(3 rows, 2.1ms)
```

When the range is empty:
```
gookv> SCAN "zzz" "zzz~";
(0 rows, 0.4ms)
```

---

### 2.6 BGET (Batch Get)

Retrieve values for multiple keys in one operation. Keys are grouped by region and fetched in parallel.

**Syntax:**
```
BGET <key1> <key2> [<key3> ...];
```

**Maps to:** `RawKVClient.BatchGet(ctx, keys) ([]KvPair, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key1`, `key2`, ... | key | One or more keys to look up (at least 1 required) |

**Output:**
```
gookv> BGET user:001 user:002 user:999;
+----------+-------+
| Key      | Value |
+----------+-------+
| user:001 | alice |
| user:002 | bob   |
+----------+-------+
(2 rows, 1 not found, 1.3ms)
```

Keys not found are omitted from the output but counted in the summary.

---

### 2.7 BPUT (Batch Put)

Store multiple key-value pairs in one operation. Pairs are grouped by region and written in parallel.

**Syntax:**
```
BPUT <key1> <val1> <key2> <val2> [<key3> <val3> ...];
```

**Maps to:** `RawKVClient.BatchPut(ctx, pairs) error` where `pairs` is `[]KvPair`.

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key1`, `val1`, ... | key, value | Key-value pairs. Must be an even number of arguments (at least 2) |

**Output:**
```
gookv> BPUT k1 v1 k2 v2 k3 v3;
OK (3 pairs)
(1.5ms)
```

**Errors:**
- Odd number of arguments: `ERROR: BPUT requires an even number of key-value arguments`

---

### 2.8 BDELETE (Batch Delete)

Remove multiple keys in one operation. Keys are grouped by region and deleted in parallel.

**Syntax:**
```
BDELETE <key1> <key2> [<key3> ...];
```

**Maps to:** `RawKVClient.BatchDelete(ctx, keys) error`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key1`, `key2`, ... | key | One or more keys to delete (at least 1 required) |

**Output:**
```
gookv> BDELETE k1 k2 k3;
OK (3 keys)
(1.2ms)
```

---

### 2.9 DELETE RANGE

Delete all keys in a lexicographic range. Transparently spans region boundaries.

**Syntax:**
```
DELETE RANGE <start_key> <end_key>;
```

**Maps to:** `RawKVClient.DeleteRange(ctx, startKey, endKey) error`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `start_key` | key | Inclusive start of range |
| `end_key` | key | Exclusive end of range |

**Output:**
```
gookv> DELETE RANGE "tmp:" "tmp:~";
OK
(3.2ms)
```

**Note:** This is a destructive bulk operation. The CLI does not prompt for confirmation in non-interactive mode (piped input). In interactive mode, a warning is printed:
```
gookv> DELETE RANGE "" "";
WARNING: This will delete ALL keys. Proceed? [y/N] y
OK
(15.7ms)
```

---

### 2.10 CAS (Compare And Swap)

Atomically compare the current value and swap if it matches.

**Syntax:**
```
CAS <key> <new_value> <expected_old>;
CAS <key> <new_value> _ NOT_EXIST;
```

**Maps to:** `RawKVClient.CompareAndSwap(ctx, key, value, prevValue, prevNotExist) (bool, []byte, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to update |
| `new_value` | value | The value to write if the comparison succeeds |
| `expected_old` | value | The expected current value. Use `_` with `NOT_EXIST` to assert the key does not exist |
| `NOT_EXIST` | flag | (Optional) When present, the swap succeeds only if the key does not currently exist. The `expected_old` argument is ignored (use `_` as placeholder) |

**Output -- success:**
```
gookv> CAS counter "2" "1";
OK (swapped)
  previous: "1"
(1.0ms)
```

**Output -- failure (value mismatch):**
```
gookv> CAS counter "2" "1";
FAILED (not swapped)
  previous: "5"
(0.9ms)
```

**Output -- NOT_EXIST success:**
```
gookv> CAS newkey "initial" _ NOT_EXIST;
OK (swapped)
  previous: (not found)
(0.8ms)
```

---

### 2.11 CHECKSUM

Compute a CRC checksum over a key range. Spans region boundaries, XORing per-region checksums.

**Syntax:**
```
CHECKSUM;
CHECKSUM <start_key> <end_key>;
```

**Maps to:** `RawKVClient.Checksum(ctx, startKey, endKey) (uint64, uint64, uint64, error)` returning `(checksum, totalKvs, totalBytes)`.

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `start_key` | key | (Optional) Inclusive start of range. Default: `""` (beginning) |
| `end_key` | key | (Optional) Exclusive end of range. Default: `""` (end) |

When called without arguments, checksums the entire keyspace.

**Output:**
```
gookv> CHECKSUM "user:" "user:~";
Checksum:    0xa3f7e2b104c8d91e
Total keys:  1,247
Total bytes: 89,412
(45.3ms)

gookv> CHECKSUM;
Checksum:    0xf1a2b3c4d5e6f7a8
Total keys:  15,032
Total bytes: 1,245,678
(312.7ms)
```

---

## 3. Transactional Commands

Transactional commands operate on a `TxnHandle` obtained from `TxnKVClient.Begin()`. The REPL tracks whether a transaction is active and changes the prompt accordingly.

### 3.1 BEGIN

Start a new transaction.

**Syntax:**
```
BEGIN;
BEGIN PESSIMISTIC;
BEGIN ASYNC_COMMIT ONE_PC LOCK_TTL 5000;
```

**Maps to:** `TxnKVClient.Begin(ctx, opts ...TxnOption) (*TxnHandle, error)`

**Options:**
| Option | Maps to | Description |
|--------|---------|-------------|
| `PESSIMISTIC` | `WithPessimistic()` | Pessimistic mode: locks acquired eagerly on SET/DELETE. Default: optimistic |
| `ASYNC_COMMIT` | `WithAsyncCommit()` | Async commit: transaction is logically committed at prewrite time |
| `ONE_PC` | `With1PC()` | Attempt single-phase commit for single-region transactions |
| `LOCK_TTL <ms>` | `WithLockTTL(ms)` | Lock TTL in milliseconds. Default: 3000 |

Options can be combined in any order.

**Output:**
```
gookv> BEGIN PESSIMISTIC;
Transaction started (pessimistic, startTS=443552187662221312)

gookv(txn)> _
```

The prompt changes from `gookv>` to `gookv(txn)>` to indicate an active transaction.

**Errors:**
- Transaction already active: `ERROR: transaction already in progress; COMMIT or ROLLBACK first`
- PD unreachable (cannot allocate timestamp): `ERROR: get start timestamp: <detail>`

---

### 3.2 SET (within transaction)

Buffer a key-value write. In pessimistic mode, also acquires a pessimistic lock immediately.

**Syntax:**
```
SET <key> <value>;
```

**Maps to:** `TxnHandle.Set(ctx, key, value) error`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to write |
| `value` | value | The value to buffer |

**Output:**
```
gookv(txn)> SET "account:alice" "900";
OK

gookv(txn)> SET "account:bob" "1100";
OK
```

**Errors (pessimistic mode):**
- Write conflict: `ERROR: txn: write conflict` -- another transaction holds a conflicting lock
- Deadlock detected: `ERROR: txn: deadlock` -- circular lock dependency detected

---

### 3.3 GET (within transaction)

Read a value, checking the local mutation buffer first, then falling back to remote read at the transaction's snapshot timestamp. Locks encountered from other transactions are automatically resolved with retries.

**Syntax:**
```
GET <key>;
```

**Maps to:** `TxnHandle.Get(ctx, key) ([]byte, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to read |

**Output:**
```
gookv(txn)> GET "account:alice";
"1000"
(1 row, 1.2ms)

gookv(txn)> GET nonexistent;
(nil)
(0 rows, 0.8ms)
```

A `(nil)` result means the key does not exist at this snapshot. If the key was deleted in the current transaction's buffer, the result is also `(nil)`.

**Errors:**
- Transaction already committed: `ERROR: txn: already committed`
- Transaction already rolled back: `ERROR: txn: already rolled back`
- Lock resolution exhausted: `ERROR: get key <hex>: lock resolution retries exhausted`

---

### 3.4 DELETE (within transaction)

Buffer a delete mutation. In pessimistic mode, also acquires a pessimistic lock immediately.

**Syntax:**
```
DELETE <key>;
```

**Maps to:** `TxnHandle.Delete(ctx, key) error`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to delete |

**Output:**
```
gookv(txn)> DELETE "account:temp";
OK
```

**Errors:** Same as SET (write conflict, deadlock in pessimistic mode).

---

### 3.5 COMMIT

Commit the active transaction using the two-phase commit (2PC) protocol. For empty transactions (no mutations), the commit succeeds immediately.

**Syntax:**
```
COMMIT;
```

**Maps to:** `TxnHandle.Commit(ctx) error`

**Output:**
```
gookv(txn)> COMMIT;
OK (commitTS=443552187662221318, 2 keys)
(4.5ms)

gookv> _
```

The prompt reverts to `gookv>` after a successful commit.

**Errors:**
- Write conflict (optimistic mode, detected at prewrite): `ERROR: txn: write conflict`
- Already committed: `ERROR: txn: already committed`
- Already rolled back: `ERROR: txn: already rolled back`
- Prewrite/commit RPC failure: `ERROR: <detail>`

On commit failure (except "already committed"), the transaction remains active. The user should issue `ROLLBACK;` to clean up locks.

---

### 3.6 ROLLBACK

Abort the active transaction, rolling back all prewritten and pessimistic locks.

**Syntax:**
```
ROLLBACK;
```

**Maps to:** `TxnHandle.Rollback(ctx) error`

**Output:**
```
gookv(txn)> ROLLBACK;
OK (rolled back)

gookv> _
```

The prompt reverts to `gookv>`. Rolling back an already-rolled-back transaction is a no-op (returns success). Rolling back a committed transaction returns an error.

**Errors:**
- Already committed: `ERROR: txn: already committed`

---

### 3.7 Transaction Workflow Example

```
gookv> BEGIN PESSIMISTIC;
Transaction started (pessimistic, startTS=443552187662221312)

gookv(txn)> GET "account:alice";
"1000"
(1 row, 1.2ms)

gookv(txn)> GET "account:bob";
"1000"
(1 row, 0.9ms)

gookv(txn)> SET "account:alice" "900";
OK

gookv(txn)> SET "account:bob" "1100";
OK

gookv(txn)> COMMIT;
OK (commitTS=443552187662221318, 2 keys)
(4.5ms)

gookv> GET "account:alice";
"900"
(1 row, 0.8ms)
```

---

## 4. Administrative Commands

### 4.1 STORE LIST

List all stores registered with PD.

**Syntax:**
```
STORE LIST;
```

**Maps to:** `pdclient.Client.GetAllStores(ctx) ([]*metapb.Store, error)`

**Output:**
```
gookv> STORE LIST;
+---------+---------------------+-------+
| StoreID | Address             | State |
+---------+---------------------+-------+
|       1 | 127.0.0.1:20160     | Up    |
|       2 | 127.0.0.1:20161     | Up    |
|       3 | 127.0.0.1:20162     | Up    |
+---------+---------------------+-------+
(3 stores)
```

---

### 4.2 STORE STATUS

Show details for a specific store.

**Syntax:**
```
STORE STATUS <store_id>;
```

**Maps to:** `pdclient.Client.GetStore(ctx, storeID) (*metapb.Store, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `store_id` | integer | The store ID to inspect |

**Output:**
```
gookv> STORE STATUS 1;
Store ID:  1
Address:   127.0.0.1:20160
State:     Up
(0.4ms)
```

**Errors:**
- Store not found: `ERROR: store 99 not found`

---

### 4.3 REGION

Look up the region containing a given key.

**Syntax:**
```
REGION <key>;
```

**Maps to:** `pdclient.Client.GetRegion(ctx, key) (*metapb.Region, *metapb.Peer, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `key` | key | The key to locate |

**Output:**
```
gookv> REGION "account:alice";
Region ID:  2
  StartKey:  6163636f756e743a (account:)
  EndKey:    6163636f756e743a6d (account:m)
  Epoch:     conf_ver:1 version:3
  Peers:     [store:1, store:2, store:3]
  Leader:    store:2
(0.6ms)
```

---

### 4.4 REGION LIST

Walk the key space to enumerate all regions.

**Syntax:**
```
REGION LIST;
REGION LIST LIMIT <n>;
```

**Maps to:** Repeated calls to `pdclient.Client.GetRegion(ctx, key)` starting from `""`, advancing by each region's `EndKey`. Default limit: 100.

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `n` | integer | (Optional) Maximum regions to display. Default: 100 |

**Output:**
```
gookv> REGION LIST LIMIT 5;
+----------+-------------------+-------------------+---------+--------+
| RegionID | StartKey          | EndKey            | Peers   | Leader |
+----------+-------------------+-------------------+---------+--------+
|        1 | (empty)           | 6163636f756e743a  | 1,2,3   |      1 |
|        2 | 6163636f756e743a  | 6163636f756e743a6d| 1,2,3   |      2 |
|        3 | 6163636f756e743a6d| 757365723a        | 1,2,3   |      3 |
|        4 | 757365723a        | (empty)           | 1,2,3   |      1 |
+----------+-------------------+-------------------+---------+--------+
(4 regions)
```

An `(empty)` EndKey indicates the last region (unbounded upper end).

---

### 4.5 CLUSTER INFO

Show a combined overview of the cluster: cluster ID, stores, and region count.

**Syntax:**
```
CLUSTER INFO;
```

**Maps to:** Combines `pdclient.Client.GetClusterID(ctx)`, `GetAllStores(ctx)`, and a region walk via `GetRegion`.

**Output:**
```
gookv> CLUSTER INFO;
Cluster ID: 1

Stores:
+---------+---------------------+-------+
| StoreID | Address             | State |
+---------+---------------------+-------+
|       1 | 127.0.0.1:20160     | Up    |
|       2 | 127.0.0.1:20161     | Up    |
|       3 | 127.0.0.1:20162     | Up    |
+---------+---------------------+-------+

Regions: 4 total
(5.2ms)
```

---

### 4.6 TSO

Allocate and display a new timestamp from PD's Timestamp Oracle.

**Syntax:**
```
TSO;
```

**Maps to:** `pdclient.Client.GetTS(ctx) (TimeStamp, error)`

The timestamp is encoded as `physical<<18 | logical` (compatible with TiKV's format). The physical component is milliseconds since the Unix epoch.

**Output:**
```
gookv> TSO;
Timestamp:  443552187662221312
  physical: 1690000000000 (2023-07-22T06:13:20Z)
  logical:  0
(0.3ms)
```

---

### 4.7 GC SAFEPOINT

Query the cluster-wide GC safe point from PD.

**Syntax:**
```
GC SAFEPOINT;
```

**Maps to:** `pdclient.Client.GetGCSafePoint(ctx) (uint64, error)`

**Output:**
```
gookv> GC SAFEPOINT;
GC SafePoint: 443552100000000000
  physical:   1689999000000 (2023-07-22T05:56:40Z)
  logical:    0
(0.4ms)
```

If the safe point has never been set, the value is 0:
```
gookv> GC SAFEPOINT;
GC SafePoint: 0 (not set)
(0.3ms)
```

---

### 4.8 STATUS

Query a gookv-server's HTTP status endpoint.

**Syntax:**
```
STATUS;
STATUS <store_addr>;
```

**Maps to:** HTTP GET to `http://<status_addr>/status`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `store_addr` | string | (Optional) The status HTTP address of the server (e.g., `127.0.0.1:20180`). If omitted, uses the connected server's status address |

**Output:**
```
gookv> STATUS 127.0.0.1:20180;
{
  "status": "ok",
  "store_id": 1,
  "address": "127.0.0.1:20160"
}
(0.2ms)
```

**Errors:**
- Connection refused: `ERROR: status request failed: connection refused`

---

## 5. Meta Commands

### 5.1 HELP

Display a summary of all available commands.

**Syntax:**
```
HELP;
```

**Output:**
```
gookv> HELP;
Raw KV Commands:
  GET <key>                          Get a value
  PUT <key> <value> [TTL <sec>]      Put a value (optional TTL)
  DELETE <key>                       Delete a key
  SCAN <start> <end> [LIMIT <n>]    Scan a key range
  BGET <k1> <k2> ...                Batch get
  BPUT <k1> <v1> <k2> <v2> ...      Batch put
  BDELETE <k1> <k2> ...             Batch delete
  DELETE RANGE <start> <end>         Delete a key range
  TTL <key>                          Get remaining TTL
  CAS <key> <new> <old> [NOT_EXIST]  Compare and swap
  CHECKSUM [<start> <end>]           Compute range checksum

Transaction Commands:
  BEGIN [options]                     Start a transaction
  SET <key> <value>                  Set within transaction
  GET <key>                          Get within transaction
  DELETE <key>                       Delete within transaction
  COMMIT                             Commit transaction
  ROLLBACK                           Rollback transaction

Admin Commands:
  STORE LIST                         List all stores
  STORE STATUS <id>                  Show store details
  REGION <key>                       Find region for key
  REGION LIST [LIMIT <n>]            List all regions
  CLUSTER INFO                       Show cluster overview
  TSO                                Allocate a timestamp
  GC SAFEPOINT                       Show GC safe point
  STATUS [<addr>]                    Query server status

Meta Commands:
  HELP                               Show this help
  EXIT / QUIT                        Exit the REPL
  SET TIMING ON|OFF                  Toggle timing display
  SET FORMAT TABLE|PLAIN|HEX         Set output format
```

---

### 5.2 EXIT / QUIT

Exit the REPL.

**Syntax:**
```
EXIT;
QUIT;
```

If a transaction is active, the REPL warns before exiting:
```
gookv(txn)> EXIT;
WARNING: Active transaction will be rolled back. Exit? [y/N] y
Rolling back...
Goodbye.
```

Ctrl+D (EOF) also exits, with the same rollback behavior.

---

### 5.3 SET TIMING

Toggle execution timing display for all subsequent commands.

**Syntax:**
```
SET TIMING ON;
SET TIMING OFF;
```

When `ON`, every command output includes elapsed time. When `OFF`, timing is suppressed. Default: `ON`.

**Output:**
```
gookv> SET TIMING OFF;
Timing: OFF

gookv> GET mykey;
"hello"
```

---

### 5.4 SET FORMAT

Change the output format for tabular results (SCAN, BGET, REGION LIST, etc.).

**Syntax:**
```
SET FORMAT TABLE;
SET FORMAT PLAIN;
SET FORMAT HEX;
```

| Format | Description |
|--------|-------------|
| `TABLE` | ASCII table with borders and column alignment (default) |
| `PLAIN` | Tab-separated key-value pairs, one per line. Suitable for scripting |
| `HEX` | Like PLAIN, but keys and values are always hex-encoded |

**Example -- TABLE (default):**
```
+----------+--------+
| Key      | Value  |
+----------+--------+
| user:001 | alice  |
| user:002 | bob    |
+----------+--------+
```

**Example -- PLAIN:**
```
user:001	alice
user:002	bob
```

**Example -- HEX:**
```
757365723a303031	616c696365
757365723a303032	626f62
```

---

## 6. Key/Value Encoding

### 6.1 Input Encoding

| Syntax | Description | Example |
|--------|-------------|---------|
| Unquoted token | Treated as UTF-8 bytes. Cannot contain spaces, `;`, or `"` | `mykey`, `user:001` |
| Double-quoted string | UTF-8 string with spaces and special characters. Supports `\\`, `\"`, `\n`, `\t` escapes | `"hello world"`, `"line1\nline2"` |
| Hex literal (`x'...'`) | Raw byte sequence from hex | `x'48454C4C4F'` = `HELLO` |
| Hex literal (`0x...`) | Raw byte sequence from hex | `0x48454C4C4F` = `HELLO` |

### 6.2 Output Encoding

By default (TABLE and PLAIN formats):
- Values containing only printable ASCII (0x20--0x7E) are displayed as UTF-8 strings.
- Values containing non-printable bytes are displayed as hex with a `(hex)` suffix.

```
gookv> GET binary_key;
"48656c6c6f00776f726c64" (hex)
```

In HEX format, all keys and values are always hex-encoded regardless of content.

### 6.3 Encoding Examples

```
gookv> PUT "hello world" "value with spaces";
OK

gookv> PUT x'DEADBEEF' 0xFF00FF;
OK

gookv> GET x'DEADBEEF';
"ff00ff" (hex)

gookv> SCAN x'00' x'FF' LIMIT 10;
+----------+-----------------------+
| Key      | Value                 |
+----------+-----------------------+
| hello... | value with spaces     |
| deadbeef | ff00ff (hex)          |
+----------+-----------------------+
```

### 6.4 Empty Keys and Values

- An empty key (`""`) as a range boundary means the start or end of the entire keyspace.
- An empty value (`""`) is valid and stores a zero-length byte slice.
- Commands that require a non-empty key (GET, PUT, DELETE, etc.) reject `""` with: `ERROR: key must not be empty`.

---

## Appendix: Command-to-API Mapping Summary

| Command | API Method | Return Type |
|---------|-----------|-------------|
| `GET <key>` (raw) | `RawKVClient.Get(ctx, key)` | `([]byte, bool, error)` |
| `PUT <key> <val>` | `RawKVClient.Put(ctx, key, val)` | `error` |
| `PUT <key> <val> TTL <s>` | `RawKVClient.PutWithTTL(ctx, key, val, s)` | `error` |
| `DELETE <key>` (raw) | `RawKVClient.Delete(ctx, key)` | `error` |
| `TTL <key>` | `RawKVClient.GetKeyTTL(ctx, key)` | `(uint64, error)` |
| `SCAN <s> <e> LIMIT <n>` | `RawKVClient.Scan(ctx, s, e, n)` | `([]KvPair, error)` |
| `BGET <keys...>` | `RawKVClient.BatchGet(ctx, keys)` | `([]KvPair, error)` |
| `BPUT <pairs...>` | `RawKVClient.BatchPut(ctx, pairs)` | `error` |
| `BDELETE <keys...>` | `RawKVClient.BatchDelete(ctx, keys)` | `error` |
| `DELETE RANGE <s> <e>` | `RawKVClient.DeleteRange(ctx, s, e)` | `error` |
| `CAS <k> <new> <old>` | `RawKVClient.CompareAndSwap(ctx, k, new, old, false)` | `(bool, []byte, error)` |
| `CAS <k> <new> _ NOT_EXIST` | `RawKVClient.CompareAndSwap(ctx, k, new, nil, true)` | `(bool, []byte, error)` |
| `CHECKSUM [<s> <e>]` | `RawKVClient.Checksum(ctx, s, e)` | `(uint64, uint64, uint64, error)` |
| `BEGIN [opts]` | `TxnKVClient.Begin(ctx, opts...)` | `(*TxnHandle, error)` |
| `SET <key> <val>` (txn) | `TxnHandle.Set(ctx, key, val)` | `error` |
| `GET <key>` (txn) | `TxnHandle.Get(ctx, key)` | `([]byte, error)` |
| `DELETE <key>` (txn) | `TxnHandle.Delete(ctx, key)` | `error` |
| `COMMIT` | `TxnHandle.Commit(ctx)` | `error` |
| `ROLLBACK` | `TxnHandle.Rollback(ctx)` | `error` |
| `STORE LIST` | `pdclient.Client.GetAllStores(ctx)` | `([]*metapb.Store, error)` |
| `STORE STATUS <id>` | `pdclient.Client.GetStore(ctx, id)` | `(*metapb.Store, error)` |
| `REGION <key>` | `pdclient.Client.GetRegion(ctx, key)` | `(*metapb.Region, *metapb.Peer, error)` |
| `REGION LIST` | Iterated `GetRegion` calls | -- |
| `CLUSTER INFO` | `GetClusterID` + `GetAllStores` + region walk | -- |
| `TSO` | `pdclient.Client.GetTS(ctx)` | `(TimeStamp, error)` |
| `GC SAFEPOINT` | `pdclient.Client.GetGCSafePoint(ctx)` | `(uint64, error)` |
| `STATUS [<addr>]` | HTTP GET `/status` | JSON response |

---

## Addendum: Review Feedback Incorporated

This addendum resolves all blocking and should-fix issues identified in `review_notes.md`. The original text above is preserved unchanged; the corrections below supersede any conflicting content.

### A1. SET Keyword Collision Resolved (resolves [Impl-1])

The `SET` keyword is reserved exclusively for transactional writes (`TxnHandle.Set`). The `SET TIMING` and `SET FORMAT` meta commands defined in Sections 5.3 and 5.4 above are **replaced** by backslash-prefixed meta commands that do not require semicolons:

| Old (superseded) | New (canonical) |
|---|---|
| `SET TIMING ON;` / `SET TIMING OFF;` | `\timing on` / `\timing off` |
| `SET FORMAT TABLE;` / `SET FORMAT PLAIN;` / `SET FORMAT HEX;` | `\format table` / `\format plain` / `\format hex` |

Additional meta commands:

| Command | Description |
|---------|-------------|
| `\help`, `\h`, `\?` | Print command reference |
| `\quit`, `\q` | Exit the CLI |
| `\pagesize <n>` | Set default SCAN display limit (default: 100) |

The keyword aliases `HELP;`, `EXIT;`, `QUIT;` still work (require semicolons). The EBNF `meta_cmd` production in Section 1 is updated:

```ebnf
(* Meta commands -- processed by the REPL layer before the parser.
   Backslash forms do not require semicolons.
   Keyword aliases HELP, EXIT, QUIT require semicolons. *)
meta_cmd        = help_cmd | exit_cmd ;
help_cmd        = "HELP" ;
exit_cmd        = "EXIT" | "QUIT" ;

(* Backslash meta commands are outside the formal grammar:
   \help, \h, \?       -- help
   \quit, \q           -- exit
   \timing on|off      -- toggle timing
   \format table|plain|hex -- set output format
   \pagesize <n>       -- set default SCAN limit
*)
```

This eliminates the parsing ambiguity where `SET TIMING ON;` inside a transaction would conflict with `TxnHandle.Set(key="TIMING", value="ON")`.

### A2. REGION ID Command (resolves [C-3])

This command was listed in `01_overview.md` but missing from this document. Added here:

#### REGION ID

Look up a region by its numeric ID.

**Syntax:**
```
REGION ID <region_id>;
```

**Maps to:** `pdclient.Client.GetRegionByID(ctx, regionID) (*metapb.Region, *metapb.Peer, error)`

**Parameters:**
| Parameter | Type | Description |
|-----------|------|-------------|
| `region_id` | integer | The region ID to look up |

**Output:**
```
gookv> REGION ID 2;
Region ID:  2
  StartKey:  6163636f756e743a (account:)
  EndKey:    6163636f756e743a6d (account:m)
  Epoch:     conf_ver:1 version:3
  Peers:     [store:1, store:2, store:3]
  Leader:    store:2
(0.5ms)
```

**Errors:**
- Region not found: `ERROR: region 99 not found`

The EBNF `admin_cmd` production is updated to include:
```ebnf
region_id_cmd   = "REGION" "ID" integer ;
admin_cmd       = ... | region_id_cmd | ... ;
```

### A3. TxnHandle.Get Not-Found Semantics (resolves [Impl-3])

`TxnHandle.Get(ctx, key)` returns `([]byte, error)` -- there is no separate `bool` not-found indicator (unlike `RawKVClient.Get` which returns `([]byte, bool, error)`).

**Not-found detection:** When `TxnHandle.Get` returns `(nil, nil)` (nil value with nil error), the key was not found at this snapshot (or was deleted in the transaction buffer).

**Display:** Not-found inside a transaction displays as `(nil)`, matching Redis convention:

```
gookv(txn)> GET nonexistent;
(nil)
(0.8ms)
```

This differs from raw KV mode where not-found displays as `(not found)`.

### A4. Default SCAN Limit and Pagination (resolves [E-3])

The default SCAN limit when no `LIMIT` clause is specified is **100** rows (not 20 as originally stated in Section 2.5). This prevents accidentally flooding the terminal with large result sets.

The `\pagesize <n>` meta command adjusts the default for the session:
```
gookv> \pagesize 50
Page size: 50
```

When the server returns exactly the limit number of rows, a hint is displayed:
```
(100 rows, possibly more -- use LIMIT to increase)
```

### A5. HELP Output Updated (replaces Section 5.1 output)

The HELP output is updated to reflect all naming changes:

```
gookv> HELP;
Raw KV Commands:
  GET <key>                          Get a value
  PUT <key> <value> [TTL <sec>]      Put a value (optional TTL)
  DELETE <key>                       Delete a key
  SCAN <start> <end> [LIMIT <n>]    Scan a key range (default limit: 100)
  BGET <k1> <k2> ...                Batch get
  BPUT <k1> <v1> <k2> <v2> ...      Batch put
  BDELETE <k1> <k2> ...             Batch delete
  DELETE RANGE <start> <end>         Delete a key range
  TTL <key>                          Get remaining TTL
  CAS <key> <new> <old> [NOT_EXIST]  Compare and swap
  CHECKSUM [<start> <end>]           Compute range checksum

Transaction Commands (context-sensitive: GET/SET/DELETE dispatch automatically):
  BEGIN [PESSIMISTIC] [ASYNC_COMMIT] [ONE_PC] [LOCK_TTL <ms>]
  SET <key> <value>                  Write within transaction
  GET <key>                          Read within transaction
  DELETE <key>                       Delete within transaction
  BGET <k1> <k2> ...                Batch read within transaction
  COMMIT                             Commit transaction
  ROLLBACK                           Rollback transaction

Admin Commands:
  STORE LIST                         List all stores
  STORE STATUS <id>                  Show store details
  REGION <key>                       Find region for key
  REGION ID <id>                     Find region by ID
  REGION LIST [LIMIT <n>]            List all regions
  CLUSTER INFO                       Show cluster overview
  TSO                                Allocate a timestamp
  GC SAFEPOINT                       Show GC safe point
  STATUS [<addr>]                    Query server status

Meta Commands (no semicolon needed):
  \help, \h, \?                      Show this help
  \quit, \q                          Exit the REPL
  \timing on|off                     Toggle timing display
  \format table|plain|hex            Set output format
  \pagesize <n>                      Set default SCAN limit

Aliases (require semicolons):
  HELP;                              Same as \help
  EXIT; / QUIT;                      Same as \quit

Use 0x or x'...' prefix for hex key/value literals.
Statements are terminated with semicolons.
```
