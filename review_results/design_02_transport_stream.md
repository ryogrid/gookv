# Design Doc 02: Transport Stream Reuse and Routing Fixes

## Issues Covered

| ID | Category | Summary |
|----|----------|---------|
| Server C6 | Performance | `transport.go` `Send()` creates new gRPC stream per message |
| Server P3 | Performance | Connection pool always uses index 0, making pool useless |
| Server C1 | Correctness | Loopback routing uses source regionID for target peer |

---

## Problem

### 1. New gRPC stream created per message (Server C6)

**File:** `internal/server/transport/transport.go`, lines 78-102

```go
func (c *RaftClient) Send(storeID uint64, msg *raft_serverpb.RaftMessage) error {
    conn, err := c.getConnection(storeID)
    if err != nil {
        return fmt.Errorf("failed to get connection to store %d: %w", storeID, err)
    }

    client := tikvpb.NewTikvClient(conn)
    ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
    defer cancel()

    stream, err := client.Raft(ctx)
    if err != nil {
        return fmt.Errorf("raft stream to store %d failed: %w", storeID, err)
    }

    if err := stream.Send(msg); err != nil {
        return fmt.Errorf("raft send to store %d failed: %w", storeID, err)
    }

    if _, err := stream.CloseAndRecv(); err != nil {
        return fmt.Errorf("raft close to store %d failed: %w", storeID, err)
    }
    return nil
}
```

Every call to `Send()` performs the full cycle: open stream, send one message,
close stream. gRPC stream creation involves an HTTP/2 HEADERS frame round-trip,
adding ~1-2ms of latency per message. In a 3-node cluster with typical Raft traffic
(heartbeats every 100ms to each peer, plus proposal messages), this creates
hundreds of stream open/close cycles per second.

TiKV maintains long-lived streaming connections and batches messages over them.
The `BatchSend()` method at line 105 is closer to correct but also creates a new
stream per invocation and is never used for single messages.

### 2. Connection pool always uses index 0 (Server P3)

**File:** `internal/server/transport/transport.go`, lines 269-301

```go
func (p *connPool) get(dialTimeout time.Duration) (*grpc.ClientConn, error) {
    p.mu.Lock()
    defer p.mu.Unlock()

    // Use first available connection.
    idx := 0
    if p.conns[idx] != nil {
        return p.conns[idx], nil
    }
    // ... dials and stores at p.conns[idx] ...
}
```

The pool hardcodes `idx = 0`, so only one connection is ever used regardless of
the configured pool size. The `HashRegionForConn()` helper at line 318 exists but
is never called. The pool was created with `size: 1` at line 249:

```go
pool = newConnPool(addr, 1)  // ignores cfg.PoolSize
```

Two bugs compound:
- `newConnPool` is called with hardcoded `1` instead of `cfg.PoolSize`
- `get()` ignores the pool index entirely

### 3. Loopback routing uses source regionID (Server C1)

**File:** `internal/server/coordinator.go`, lines 770-783

```go
if toStoreID == sc.storeID {
    peerMsg := raftstore.PeerMsg{
        Type: raftstore.PeerMsgTypeRaftMessage,
        Data: msg,
    }
    if err := sc.router.Send(regionID, peerMsg); err == router.ErrMailboxFull {
        for i := 0; i < 3; i++ {
            time.Sleep(time.Millisecond)
            if err = sc.router.Send(regionID, peerMsg); err != router.ErrMailboxFull {
                break
            }
        }
    }
    return
}
```

The `regionID` parameter of `sendRaftMessage` (line 755) is the **source** region
that originated the message. When routing to a peer on the same store, the router
should use the **target** peer's region ID to find the correct mailbox. In a
pre-split scenario where all peers belong to the same region, this works by accident.
After a split, the source region ID may not match the target peer's region ID:

- Source peer (region 1) sends a Raft message to peer ID 5
- Peer ID 5 now belongs to region 2 (created by split)
- `sc.router.Send(regionID=1, ...)` tries to deliver to region 1's mailbox
- Region 1 receives a message meant for region 2, or the delivery silently fails

The function signature already has the `region *metapb.Region` parameter which
contains all peer information, but the `msg.To` is a **peer ID**, not a region ID.
The target region ID must be looked up separately.

---

## Current Code

### RaftClient struct (transport.go lines 21-27)

```go
type RaftClient struct {
    mu          sync.RWMutex
    connections map[uint64]*connPool // storeID -> connection pool
    resolver    StoreResolver
    batchSize   int
    dialTimeout time.Duration
}
```

### connPool struct (transport.go lines 35-40)

```go
type connPool struct {
    mu    sync.Mutex
    addr  string
    conns []*grpc.ClientConn
    size  int
}
```

### getConnection (transport.go lines 233-256)

```go
func (c *RaftClient) getConnection(storeID uint64) (*grpc.ClientConn, error) {
    // ... lookup/create pool ...
    pool = newConnPool(addr, 1)  // line 249: hardcoded size=1
    // ...
    return pool.get(c.dialTimeout)
}
```

### sendRaftMessage (coordinator.go lines 755-821)

```go
func (sc *StoreCoordinator) sendRaftMessage(
    regionID uint64, region *metapb.Region, fromPeerID uint64, msg *raftpb.Message,
) {
    var toStoreID uint64
    for _, p := range region.GetPeers() {
        if p.GetId() == msg.To {
            toStoreID = p.GetStoreId()
            break
        }
    }
    // ... loopback uses regionID (source) ...
    // ... remote uses sc.client.Send() ...
}
```

### Send call site (coordinator.go lines 141-143, 495-497)

```go
peer.SetSendFunc(func(msgs []raftpb.Message) {
    for i := range msgs {
        sc.sendRaftMessage(regionID, region, peerID, &msgs[i])
    }
})
```

---

## Design

### A. Long-lived gRPC stream per store

Replace the per-message stream pattern with a persistent `raftStream` that is
created once per store and reused for all messages. The stream has a dedicated
send goroutine with a buffered channel to avoid blocking the caller.

```go
// raftStream wraps a long-lived gRPC Raft stream to a single store.
type raftStream struct {
    mu       sync.Mutex
    storeID  uint64
    conn     *grpc.ClientConn
    stream   tikvpb.Tikv_RaftClient
    sendCh   chan *raft_serverpb.RaftMessage
    cancel   context.CancelFunc
    closed   atomic.Bool
}

func newRaftStream(conn *grpc.ClientConn, storeID uint64, bufSize int) (*raftStream, error) {
    ctx, cancel := context.WithCancel(context.Background())
    client := tikvpb.NewTikvClient(conn)
    stream, err := client.Raft(ctx)
    if err != nil {
        cancel()
        return nil, err
    }
    rs := &raftStream{
        storeID: storeID,
        conn:    conn,
        stream:  stream,
        sendCh:  make(chan *raft_serverpb.RaftMessage, bufSize),
        cancel:  cancel,
    }
    go rs.sendLoop()
    return rs, nil
}

func (rs *raftStream) sendLoop() {
    for msg := range rs.sendCh {
        if err := rs.stream.Send(msg); err != nil {
            slog.Warn("raft stream send failed, will reconnect",
                "store", rs.storeID, "err", err)
            rs.closed.Store(true)
            return
        }
    }
}

func (rs *raftStream) Send(msg *raft_serverpb.RaftMessage) error {
    if rs.closed.Load() {
        return fmt.Errorf("stream closed")
    }
    select {
    case rs.sendCh <- msg:
        return nil
    default:
        return fmt.Errorf("stream send buffer full for store %d", rs.storeID)
    }
}

func (rs *raftStream) Close() {
    rs.closed.Store(true)
    close(rs.sendCh)
    rs.cancel()
}
```

The `RaftClient.Send()` method changes to use the persistent stream:

```go
type RaftClient struct {
    mu          sync.RWMutex
    connections map[uint64]*connPool
    streams     map[uint64]*raftStream  // storeID -> persistent stream
    resolver    StoreResolver
    batchSize   int
    dialTimeout time.Duration
    streamBuf   int
}

func (c *RaftClient) Send(storeID uint64, msg *raft_serverpb.RaftMessage) error {
    rs, err := c.getOrCreateStream(storeID)
    if err != nil {
        return err
    }
    return rs.Send(msg)
}

func (c *RaftClient) getOrCreateStream(storeID uint64) (*raftStream, error) {
    c.mu.RLock()
    rs, ok := c.streams[storeID]
    c.mu.RUnlock()

    if ok && !rs.closed.Load() {
        return rs, nil
    }

    // Need to create or recreate stream.
    conn, err := c.getConnection(storeID)
    if err != nil {
        return nil, err
    }

    c.mu.Lock()
    defer c.mu.Unlock()

    // Double-check after lock.
    if rs, ok = c.streams[storeID]; ok && !rs.closed.Load() {
        return rs, nil
    }

    // Close old stream if it exists.
    if rs != nil {
        rs.Close()
    }

    rs, err = newRaftStream(conn, storeID, c.streamBuf)
    if err != nil {
        return nil, err
    }
    c.streams[storeID] = rs
    return rs, nil
}
```

### B. Connection pool with round-robin selection

Fix `getConnection` to use the configured pool size and `get()` to use round-robin
or hash-based selection.

```go
func (c *RaftClient) getConnection(storeID uint64) (*grpc.ClientConn, error) {
    c.mu.RLock()
    pool, ok := c.connections[storeID]
    c.mu.RUnlock()

    if !ok {
        addr, err := c.resolver.ResolveStore(storeID)
        if err != nil {
            return nil, fmt.Errorf("resolve store %d: %w", storeID, err)
        }
        c.mu.Lock()
        pool, ok = c.connections[storeID]
        if !ok {
            pool = newConnPool(addr, c.poolSize)  // Use configured pool size
            c.connections[storeID] = pool
        }
        c.mu.Unlock()
    }

    return pool.get(c.dialTimeout)
}
```

```go
type connPool struct {
    mu      sync.Mutex
    addr    string
    conns   []*grpc.ClientConn
    size    int
    nextIdx uint64  // round-robin counter
}

func (p *connPool) get(dialTimeout time.Duration) (*grpc.ClientConn, error) {
    p.mu.Lock()
    defer p.mu.Unlock()

    // Round-robin across pool.
    idx := int(p.nextIdx % uint64(p.size))
    p.nextIdx++

    if p.conns[idx] != nil {
        return p.conns[idx], nil
    }

    // Establish new connection.
    ctx, cancel := context.WithTimeout(context.Background(), dialTimeout)
    defer cancel()

    conn, err := grpc.DialContext(ctx, p.addr,
        grpc.WithTransportCredentials(insecure.NewCredentials()),
        grpc.WithKeepaliveParams(keepalive.ClientParameters{
            Time:                60 * time.Second,
            Timeout:             10 * time.Second,
            PermitWithoutStream: false,
        }),
        grpc.WithDefaultCallOptions(
            grpc.MaxCallRecvMsgSize(64*1024*1024),
            grpc.MaxCallSendMsgSize(64*1024*1024),
        ),
    )
    if err != nil {
        return nil, fmt.Errorf("dial %s: %w", p.addr, err)
    }

    p.conns[idx] = conn
    return conn, nil
}
```

The `RaftClientConfig` and `RaftClient` must propagate `PoolSize`:

```go
// In NewRaftClient:
return &RaftClient{
    connections: make(map[uint64]*connPool),
    streams:     make(map[uint64]*raftStream),
    resolver:    resolver,
    batchSize:   cfg.BatchSize,
    dialTimeout: cfg.DialTimeout,
    poolSize:    cfg.PoolSize,   // NEW: store the configured pool size
    streamBuf:   4096,
}
```

### C. Fix loopback routing to use target region ID

The loopback case must find the region ID that the target peer belongs to. After
a split, the target peer may be registered under a different region ID than the
source.

**Approach:** Add a `FindRegionByPeerID(peerID uint64) (uint64, bool)` method to
the router, or use the existing region metadata to determine the correct target
region.

Since the `msg.To` is a peer ID and the router is keyed by region ID, we need
a reverse mapping. The simplest correct approach is to add a peer-to-region
lookup in the router:

```go
// In router/router.go:
type Router struct {
    peers   sync.Map // regionID -> chan PeerMsg
    storeCh chan raftstore.StoreMsg
    peerMap sync.Map // peerID -> regionID  (NEW)
}

func (r *Router) Register(regionID uint64, ch chan raftstore.PeerMsg) error {
    _, loaded := r.peers.LoadOrStore(regionID, ch)
    if loaded {
        return ErrPeerAlreadyRegistered
    }
    return nil
}

// RegisterPeer adds a peerID -> regionID mapping for loopback routing.
func (r *Router) RegisterPeer(peerID, regionID uint64) {
    r.peerMap.Store(peerID, regionID)
}

// UnregisterPeer removes a peerID mapping.
func (r *Router) UnregisterPeer(peerID uint64) {
    r.peerMap.Delete(peerID)
}

// FindRegionByPeerID looks up which region a peer belongs to.
func (r *Router) FindRegionByPeerID(peerID uint64) (uint64, bool) {
    v, ok := r.peerMap.Load(peerID)
    if !ok {
        return 0, false
    }
    return v.(uint64), true
}
```

The `sendRaftMessage` loopback case then becomes:

```go
if toStoreID == sc.storeID {
    // Find the target region ID for this peer.
    targetRegionID := regionID  // fallback to source region
    if rid, ok := sc.router.FindRegionByPeerID(msg.To); ok {
        targetRegionID = rid
    }

    peerMsg := raftstore.PeerMsg{
        Type: raftstore.PeerMsgTypeRaftMessage,
        Data: msg,
    }
    if err := sc.router.Send(targetRegionID, peerMsg); err == router.ErrMailboxFull {
        for i := 0; i < 3; i++ {
            time.Sleep(time.Millisecond)
            if err = sc.router.Send(targetRegionID, peerMsg); err != router.ErrMailboxFull {
                break
            }
        }
    }
    return
}
```

The peer-to-region mapping must be maintained at:
- `BootstrapRegion()` — register all initial peers
- `AddPeer()` / split handler — register new peers
- `RemovePeer()` — unregister removed peers

---

## Implementation Steps

1. **Add `raftStream` type and `streams` map to `RaftClient`**
   - File: `internal/server/transport/transport.go`
   - Add `raftStream` struct with `sendLoop()`, `Send()`, `Close()`
   - Add `streams map[uint64]*raftStream` field to `RaftClient`
   - Add `poolSize int` and `streamBuf int` fields to `RaftClient`

2. **Rewrite `RaftClient.Send()` to use persistent streams**
   - File: `internal/server/transport/transport.go`, lines 78-102
   - Replace stream-per-call with `getOrCreateStream()` + `raftStream.Send()`
   - Add automatic reconnection when stream is closed

3. **Fix `newConnPool` call to use configured pool size**
   - File: `internal/server/transport/transport.go`, line 249
   - Change `newConnPool(addr, 1)` to `newConnPool(addr, c.poolSize)`

4. **Implement round-robin in `connPool.get()`**
   - File: `internal/server/transport/transport.go`, lines 269-301
   - Add `nextIdx uint64` field to `connPool`
   - Replace hardcoded `idx := 0` with round-robin counter

5. **Update `RaftClient.Close()` to clean up streams**
   - File: `internal/server/transport/transport.go`, lines 210-218
   - Close all `raftStream` instances in addition to connection pools

6. **Add peer-to-region mapping in router**
   - File: `internal/raftstore/router/router.go`
   - Add `peerMap sync.Map` field
   - Add `RegisterPeer()`, `UnregisterPeer()`, `FindRegionByPeerID()`

7. **Fix loopback routing in `sendRaftMessage`**
   - File: `internal/server/coordinator.go`, lines 770-783
   - Use `sc.router.FindRegionByPeerID(msg.To)` to find target region ID
   - Fall back to source `regionID` if lookup fails (pre-split compatibility)

8. **Maintain peer-to-region mapping at registration points**
   - File: `internal/server/coordinator.go`
   - In `BootstrapRegion()`: call `RegisterPeer()` for each peer in region
   - In split handler: call `RegisterPeer()` for new peers
   - In `RemovePeer()`: call `UnregisterPeer()`

---

## Test Plan

### Unit Tests

1. **TestStreamReuse**
   - Create a `RaftClient` with a mock resolver
   - Call `Send()` twice to the same store
   - Verify only one `Raft()` stream is created (not two)
   - Verify both messages are sent on the same stream

2. **TestStreamReconnectOnFailure**
   - Create a stream, then simulate stream closure (`closed.Store(true)`)
   - Call `Send()` again
   - Verify a new stream is created automatically
   - Verify the message is delivered on the new stream

3. **TestStreamBufferFull**
   - Create a stream with a small buffer (size 2)
   - Send 3 messages without consuming
   - Verify the third returns an error (buffer full)

4. **TestConnPoolRoundRobin**
   - Create a pool with size 4
   - Call `get()` 8 times
   - Verify connections are distributed across all 4 slots
   - Verify `nextIdx` wraps around correctly

5. **TestConnPoolSizeFromConfig**
   - Create `RaftClient` with `PoolSize: 4`
   - Trigger `getConnection()` for a store
   - Verify the created pool has `size == 4`
   - Verify all 4 slots are usable

6. **TestFindRegionByPeerID**
   - Register peer 10 -> region 1, peer 20 -> region 2
   - Verify `FindRegionByPeerID(10)` returns region 1
   - Verify `FindRegionByPeerID(20)` returns region 2
   - Verify `FindRegionByPeerID(99)` returns false

7. **TestLoopbackRoutingAfterSplit**
   - Register region 1 with peers [1, 2, 3]
   - Simulate split: register region 2 with peers [4, 5, 6]
   - Update peer mapping: peer 4 -> region 2
   - Send loopback message from region 1 to peer 4
   - Verify message is delivered to region 2's mailbox (not region 1's)

8. **TestLoopbackRoutingFallback**
   - Send loopback message to a peer ID not in the peer map
   - Verify fallback to source region ID
   - Verify no panic or error

### Integration Tests

9. **TestStreamPersistenceUnderLoad**
   - Start two gookv nodes
   - Send 1000 Raft messages
   - Verify stream count stays at 1 per store (not 1000)
   - Measure latency improvement vs. per-message streams

10. **TestSplitLoopbackIntegration**
    - Bootstrap a 3-node cluster with all peers on the same store
    - Trigger a region split
    - Verify Raft heartbeats reach the correct post-split peers
    - Verify no messages are misdelivered to the wrong region

---

## Addendum: Review Feedback Incorporated

**Review verdict: NEEDS REVISION -- all blocking issues addressed below.**

### Fix 1: Add `poolSize int` to `RaftClient` struct definition in Section A -- BLOCKING

The `RaftClient` struct definition in Section A only adds `streams` and `streamBuf`, but omits the `poolSize` field. The `NewRaftClient` code in Section B references `poolSize: cfg.PoolSize`, creating an inconsistency. The current `RaftClient` struct (transport.go lines 21-27) has no `poolSize` field, and `NewRaftClient` (lines 69-74) validates `cfg.PoolSize` but never stores it.

**Corrected `RaftClient` struct definition (Section A):**

```go
type RaftClient struct {
    mu          sync.RWMutex
    connections map[uint64]*connPool
    streams     map[uint64]*raftStream  // storeID -> persistent stream
    resolver    StoreResolver
    batchSize   int
    dialTimeout time.Duration
    poolSize    int   // NEW: configured connection pool size per store
    streamBuf   int   // NEW: send channel buffer size for streams
}
```

Both the struct definition and the `NewRaftClient` constructor must be updated together.

### Fix 2: Stream Close/Send race -- use context cancellation instead of closing sendCh -- BLOCKING

The `raftStream.Close()` method as designed calls `close(rs.sendCh)` and then `rs.cancel()`. There is a race: if `Send()` passes the `closed.Load()` check and then `Close()` runs concurrently, `Send()` will attempt to write to a closed channel, causing a panic.

**Corrected approach**: Never close `sendCh`. Use context cancellation to stop `sendLoop`:

```go
func (rs *raftStream) sendLoop() {
    for {
        select {
        case msg := <-rs.sendCh:
            if err := rs.stream.Send(msg); err != nil {
                slog.Warn("raft stream send failed, will reconnect",
                    "store", rs.storeID, "err", err)
                rs.closed.Store(true)
                return
            }
        case <-rs.ctx.Done():
            return
        }
    }
}

func (rs *raftStream) Close() {
    rs.closed.Store(true)
    rs.cancel()  // context cancellation unblocks sendLoop
    // Do NOT close(rs.sendCh) -- avoids race with concurrent Send()
}
```

The `Send()` method uses the existing `closed.Load()` check plus a select-based send with the context:

```go
func (rs *raftStream) Send(msg *raft_serverpb.RaftMessage) error {
    if rs.closed.Load() {
        return fmt.Errorf("stream closed")
    }
    select {
    case rs.sendCh <- msg:
        return nil
    default:
        return fmt.Errorf("stream send buffer full for store %d", rs.storeID)
    }
}
```

This is safe because `sendCh` is never closed, so writes to it cannot panic. The `sendLoop` goroutine exits via context cancellation.

**Note on stream reconnection**: When `getOrCreateStream` detects a closed stream and creates a new one, buffered messages in the old `sendCh` are dropped. This is acceptable because Raft is idempotent and retries unacknowledged messages, but this trade-off should be documented in code comments.

### Fix 3: Add CreatePeer/DestroyPeer/ConfChange as peer-to-region mapping maintenance points

The design lists three maintenance points for `RegisterPeer`/`UnregisterPeer` but misses three others:

**Updated maintenance points (Section C):**

- `BootstrapRegion()` -- register all initial peers (already listed)
- `AddPeer()` / split handler -- register new peers (already listed)
- `RemovePeer()` -- unregister removed peers (already listed)
- **`CreatePeer()` (coordinator.go line ~488)** -- when `maybeCreatePeerForMessage` creates a new peer, call `RegisterPeer()` to update the mapping
- **`DestroyPeer()` (coordinator.go)** -- when a peer is destroyed, call `UnregisterPeer()` to clean up the mapping
- **ConfChange application** -- when peers are added or removed via Raft config changes, update the mapping accordingly

Without these additional maintenance points, the peer-to-region mapping can become stale, causing loopback messages to be misrouted. The fallback to source `regionID` mitigates this for pre-split scenarios but does not cover all cases.

### Fix 4: Add `RaftClient.Close()` code that closes streams

Implementation Step 5 mentions updating `Close()` but the code was not provided. The current `Close()` (transport.go lines 210-218) only closes connection pools. Here is the required implementation:

```go
func (c *RaftClient) Close() {
    c.mu.Lock()
    defer c.mu.Unlock()
    // Close all persistent streams first.
    for _, rs := range c.streams {
        rs.Close()
    }
    c.streams = make(map[uint64]*raftStream)
    // Then close connection pools.
    for _, pool := range c.connections {
        pool.close()
    }
    c.connections = make(map[uint64]*connPool)
}
```

### Minor: Connection pool round-robin vs. long-lived streams interaction

The design adds both connection pool round-robin and long-lived streams. A stream is bound to one connection from the pool. If the pool has multiple connections, the stream only uses one. The round-robin in the pool is useful for `BatchSend` and `SendSnapshot`, but the single stream per store makes the pool less relevant for normal `Send`. This is not a bug but the interaction should be documented in implementation comments.
