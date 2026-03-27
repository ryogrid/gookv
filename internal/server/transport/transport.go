// Package transport implements the inter-node Raft message transport for gookv.
// It provides connection pooling, message batching, and snapshot transfer via gRPC.
package transport

import (
	"context"
	"fmt"
	"hash/fnv"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
)

// RaftClient manages gRPC connections to other gookv nodes for Raft message transport.
type RaftClient struct {
	mu          sync.RWMutex
	connections map[uint64]*connPool    // storeID -> connection pool
	streams     map[uint64]*raftStream  // storeID -> persistent stream
	resolver    StoreResolver
	batchSize   int
	dialTimeout time.Duration
	poolSize    int // configured connection pool size per store
	streamBuf   int // send channel buffer size for streams
}

// StoreResolver resolves a store ID to a network address.
type StoreResolver interface {
	ResolveStore(storeID uint64) (string, error)
}

// connPool manages a pool of gRPC connections to a single store.
type connPool struct {
	mu      sync.Mutex
	addr    string
	conns   []*grpc.ClientConn
	size    int
	nextIdx uint64 // round-robin counter
}

// RaftClientConfig configures the RaftClient.
type RaftClientConfig struct {
	PoolSize    int           // Number of connections per store (default 1)
	BatchSize   int           // Max messages per batch send (default 128)
	DialTimeout time.Duration // Connection timeout (default 5s)
}

// DefaultRaftClientConfig returns sensible defaults.
func DefaultRaftClientConfig() RaftClientConfig {
	return RaftClientConfig{
		PoolSize:    1,
		BatchSize:   128,
		DialTimeout: 5 * time.Second,
	}
}

// NewRaftClient creates a new RaftClient.
func NewRaftClient(resolver StoreResolver, cfg RaftClientConfig) *RaftClient {
	if cfg.PoolSize <= 0 {
		cfg.PoolSize = 1
	}
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 128
	}
	if cfg.DialTimeout <= 0 {
		cfg.DialTimeout = 5 * time.Second
	}
	return &RaftClient{
		connections: make(map[uint64]*connPool),
		streams:     make(map[uint64]*raftStream),
		resolver:    resolver,
		batchSize:   cfg.BatchSize,
		dialTimeout: cfg.DialTimeout,
		poolSize:    cfg.PoolSize,
		streamBuf:   4096,
	}
}

// Send sends a Raft message to the target store via a persistent gRPC streaming RPC.
// The stream is created lazily on first use and reused for subsequent messages.
// If the stream is closed (e.g., due to a send error), it is automatically recreated.
func (c *RaftClient) Send(storeID uint64, msg *raft_serverpb.RaftMessage) error {
	rs, err := c.getOrCreateStream(storeID)
	if err != nil {
		return err
	}
	return rs.send(msg)
}

// getOrCreateStream returns an existing persistent stream for the store,
// or creates a new one if none exists or the existing one is closed.
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
		rs.close()
	}

	rs, err = newRaftStream(conn, storeID, c.streamBuf)
	if err != nil {
		return nil, err
	}
	c.streams[storeID] = rs
	return rs, nil
}

// BatchSend sends multiple Raft messages to the target store.
func (c *RaftClient) BatchSend(storeID uint64, msgs []*raft_serverpb.RaftMessage) error {
	if len(msgs) == 0 {
		return nil
	}

	conn, err := c.getConnection(storeID)
	if err != nil {
		return fmt.Errorf("failed to get connection to store %d: %w", storeID, err)
	}

	client := tikvpb.NewTikvClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	slog.Debug("raft.BatchSend", "store-id", storeID, "msgs", len(msgs))
	stream, err := client.BatchRaft(ctx)
	if err != nil {
		return fmt.Errorf("batch raft stream to store %d failed: %w", storeID, err)
	}

	batch := &tikvpb.BatchRaftMessage{}
	for _, msg := range msgs {
		batch.Msgs = append(batch.Msgs, msg)
		if len(batch.Msgs) >= c.batchSize {
			if err := stream.Send(batch); err != nil {
				return fmt.Errorf("batch send to store %d failed: %w", storeID, err)
			}
			batch = &tikvpb.BatchRaftMessage{}
		}
	}

	// Send remaining messages.
	if len(batch.Msgs) > 0 {
		if err := stream.Send(batch); err != nil {
			return fmt.Errorf("batch send to store %d failed: %w", storeID, err)
		}
	}

	if _, err := stream.CloseAndRecv(); err != nil {
		return fmt.Errorf("batch raft close to store %d failed: %w", storeID, err)
	}

	return nil
}

// SendSnapshot sends a snapshot to the target store.
func (c *RaftClient) SendSnapshot(storeID uint64, msg *raft_serverpb.RaftMessage, data []byte) error {
	conn, err := c.getConnection(storeID)
	if err != nil {
		return fmt.Errorf("failed to get connection to store %d: %w", storeID, err)
	}

	client := tikvpb.NewTikvClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	slog.Debug("raft.Snapshot", "store-id", storeID)
	stream, err := client.Snapshot(ctx)
	if err != nil {
		return fmt.Errorf("snapshot stream to store %d failed: %w", storeID, err)
	}

	// Send chunks of 1MB.
	const chunkSize = 1024 * 1024
	chunk := &raft_serverpb.SnapshotChunk{
		Message: msg,
	}

	// Send the first chunk with the Raft message metadata.
	if len(data) <= chunkSize {
		chunk.Data = data
		if err := stream.Send(chunk); err != nil {
			return fmt.Errorf("snapshot send to store %d failed: %w", storeID, err)
		}
	} else {
		// Send metadata chunk first.
		chunk.Data = data[:chunkSize]
		if err := stream.Send(chunk); err != nil {
			return fmt.Errorf("snapshot send to store %d failed: %w", storeID, err)
		}

		// Send remaining data chunks.
		for offset := chunkSize; offset < len(data); offset += chunkSize {
			end := offset + chunkSize
			if end > len(data) {
				end = len(data)
			}
			dataChunk := &raft_serverpb.SnapshotChunk{
				Data: data[offset:end],
			}
			if err := stream.Send(dataChunk); err != nil {
				return fmt.Errorf("snapshot chunk send to store %d failed: %w", storeID, err)
			}
		}
	}

	resp, err := stream.CloseAndRecv()
	if err != nil {
		return fmt.Errorf("snapshot close to store %d failed: %w", storeID, err)
	}
	_ = resp
	return nil
}

// Close closes all persistent streams and connections.
func (c *RaftClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Close all persistent streams first.
	for _, rs := range c.streams {
		rs.close()
	}
	c.streams = make(map[uint64]*raftStream)

	// Then close connection pools.
	for _, pool := range c.connections {
		pool.close()
	}
	c.connections = make(map[uint64]*connPool)
}

// RemoveConnection removes and closes the stream and connections to a specific store.
func (c *RaftClient) RemoveConnection(storeID uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Close the persistent stream first.
	if rs, ok := c.streams[storeID]; ok {
		rs.close()
		delete(c.streams, storeID)
	}

	if pool, ok := c.connections[storeID]; ok {
		pool.close()
		delete(c.connections, storeID)
	}
}

// getConnection returns a gRPC connection for the given store, using consistent hashing
// across the connection pool. It establishes new connections lazily.
func (c *RaftClient) getConnection(storeID uint64) (*grpc.ClientConn, error) {
	c.mu.RLock()
	pool, ok := c.connections[storeID]
	c.mu.RUnlock()

	if !ok {
		// Resolve address and create pool.
		addr, err := c.resolver.ResolveStore(storeID)
		if err != nil {
			return nil, fmt.Errorf("resolve store %d: %w", storeID, err)
		}

		c.mu.Lock()
		// Double-check after acquiring write lock.
		pool, ok = c.connections[storeID]
		if !ok {
			pool = newConnPool(addr, c.poolSize)
			c.connections[storeID] = pool
		}
		c.mu.Unlock()
	}

	return pool.get(c.dialTimeout)
}

func newConnPool(addr string, size int) *connPool {
	if size <= 0 {
		size = 1
	}
	return &connPool{
		addr:  addr,
		conns: make([]*grpc.ClientConn, size),
		size:  size,
	}
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
	slog.Debug("raft.dial", "addr", p.addr)
	conn, err := grpc.NewClient(p.addr,
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

func (p *connPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()

	for i, conn := range p.conns {
		if conn != nil {
			conn.Close()
			p.conns[i] = nil
		}
	}
}

// HashRegionForConn selects a connection index based on region ID.
// Uses seahash-like FNV for simplicity.
func HashRegionForConn(regionID uint64, poolSize int) int {
	if poolSize <= 1 {
		return 0
	}
	h := fnv.New64a()
	var buf [8]byte
	buf[0] = byte(regionID)
	buf[1] = byte(regionID >> 8)
	buf[2] = byte(regionID >> 16)
	buf[3] = byte(regionID >> 24)
	buf[4] = byte(regionID >> 32)
	buf[5] = byte(regionID >> 40)
	buf[6] = byte(regionID >> 48)
	buf[7] = byte(regionID >> 56)
	h.Write(buf[:])
	return int(h.Sum64() % uint64(poolSize))
}

// raftStream wraps a long-lived gRPC Raft stream to a single store.
// It has a dedicated send goroutine with a buffered channel to avoid blocking
// the caller. When a send error occurs, the stream is marked closed and
// getOrCreateStream will recreate it on the next Send(). Buffered messages
// in the old sendCh are dropped; this is safe because Raft retries
// unacknowledged messages.
type raftStream struct {
	storeID uint64
	conn    *grpc.ClientConn
	stream  tikvpb.Tikv_RaftClient
	sendCh  chan *raft_serverpb.RaftMessage
	ctx     context.Context
	cancel  context.CancelFunc
	closed  atomic.Bool
}

func newRaftStream(conn *grpc.ClientConn, storeID uint64, bufSize int) (*raftStream, error) {
	ctx, cancel := context.WithCancel(context.Background())
	client := tikvpb.NewTikvClient(conn)
	stream, err := client.Raft(ctx)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("raft stream to store %d failed: %w", storeID, err)
	}
	rs := &raftStream{
		storeID: storeID,
		conn:    conn,
		stream:  stream,
		sendCh:  make(chan *raft_serverpb.RaftMessage, bufSize),
		ctx:     ctx,
		cancel:  cancel,
	}
	go rs.sendLoop()
	return rs, nil
}

// sendLoop drains sendCh and writes to the gRPC stream. It exits when the
// context is cancelled or a send error occurs.
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

// send enqueues a message on the stream's send channel. Returns an error if the
// stream is closed or the send buffer is full.
func (rs *raftStream) send(msg *raft_serverpb.RaftMessage) error {
	if rs.closed.Load() {
		return fmt.Errorf("stream to store %d closed", rs.storeID)
	}
	select {
	case rs.sendCh <- msg:
		return nil
	default:
		return fmt.Errorf("stream send buffer full for store %d", rs.storeID)
	}
}

// close marks the stream as closed and cancels the context to stop sendLoop.
// The sendCh is intentionally NOT closed to avoid a race with concurrent send().
func (rs *raftStream) close() {
	rs.closed.Store(true)
	rs.cancel()
}

// MessageBatcher accumulates Raft messages and flushes them in batches.
type MessageBatcher struct {
	mu      sync.Mutex
	batches map[uint64][]*raft_serverpb.RaftMessage // storeID -> pending messages
	client  *RaftClient
	maxSize int
}

// NewMessageBatcher creates a batcher that flushes at maxSize messages.
func NewMessageBatcher(client *RaftClient, maxSize int) *MessageBatcher {
	if maxSize <= 0 {
		maxSize = 128
	}
	return &MessageBatcher{
		batches: make(map[uint64][]*raft_serverpb.RaftMessage),
		client:  client,
		maxSize: maxSize,
	}
}

// Add adds a message to the batch for the target store.
func (b *MessageBatcher) Add(storeID uint64, msg *raft_serverpb.RaftMessage) {
	b.mu.Lock()
	defer b.mu.Unlock()

	b.batches[storeID] = append(b.batches[storeID], msg)
}

// Flush sends all pending batches.
func (b *MessageBatcher) Flush() map[uint64]error {
	b.mu.Lock()
	batches := b.batches
	b.batches = make(map[uint64][]*raft_serverpb.RaftMessage)
	b.mu.Unlock()

	errs := make(map[uint64]error)
	for storeID, msgs := range batches {
		if err := b.client.BatchSend(storeID, msgs); err != nil {
			errs[storeID] = err
		}
	}
	return errs
}

// Pending returns the number of pending messages per store.
func (b *MessageBatcher) Pending() map[uint64]int {
	b.mu.Lock()
	defer b.mu.Unlock()

	result := make(map[uint64]int)
	for storeID, msgs := range b.batches {
		result[storeID] = len(msgs)
	}
	return result
}
