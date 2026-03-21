package pd

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/pingcap/kvproto/pkg/pdpb"
)

const tsoBatchSize = 1000

// TSOBuffer pre-allocates TSO ranges via Raft and serves subsequent requests
// from a local buffer. This amortizes the cost of Raft consensus over many
// TSO allocations. On leader change, the buffer must be reset so the new
// leader proposes a fresh batch.
type TSOBuffer struct {
	mu       sync.Mutex
	physical int64
	logical  int64
	remain   int
	raftPeer *PDRaftPeer
}

// NewTSOBuffer creates a TSOBuffer backed by the given Raft peer.
func NewTSOBuffer(raftPeer *PDRaftPeer) *TSOBuffer {
	return &TSOBuffer{
		raftPeer: raftPeer,
	}
}

// GetTS allocates count timestamps from the buffer. If the buffer is depleted,
// it proposes a new CmdTSOAllocate batch via Raft.
// Returns a *pdpb.Timestamp representing the last allocated timestamp.
func (b *TSOBuffer) GetTS(ctx context.Context, count int) (*pdpb.Timestamp, error) {
	if count <= 0 {
		count = 1
	}

	b.mu.Lock()
	defer b.mu.Unlock()

	if b.remain < count {
		// Refill the buffer via Raft.
		batchSize := tsoBatchSize
		if count > batchSize {
			batchSize = count
		}
		if err := b.refill(ctx, batchSize); err != nil {
			return nil, err
		}
	}

	// Serve from local buffer.
	b.logical += int64(count)
	b.remain -= count

	return &pdpb.Timestamp{
		Physical: b.physical,
		Logical:  b.logical,
	}, nil
}

// refill proposes a CmdTSOAllocate via Raft and stores the result as the
// upper bound of the local buffer. Must be called with b.mu held.
func (b *TSOBuffer) refill(ctx context.Context, batchSize int) error {
	cmd := PDCommand{
		Type:         CmdTSOAllocate,
		TSOBatchSize: batchSize,
	}

	result, err := b.raftPeer.ProposeAndWait(ctx, cmd)
	if err != nil {
		return fmt.Errorf("tso buffer: propose: %w", err)
	}

	var ts pdpb.Timestamp
	if err := json.Unmarshal(result, &ts); err != nil {
		return fmt.Errorf("tso buffer: unmarshal timestamp: %w", err)
	}

	// The returned timestamp is the upper bound (last allocated in the batch).
	// Set up the buffer to serve from [upper - batchSize + 1, upper].
	b.physical = ts.Physical
	b.logical = ts.Logical - int64(batchSize)
	b.remain = batchSize

	return nil
}

// Reset clears the buffer. Must be called on leader change so the new leader
// proposes a fresh batch.
func (b *TSOBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.physical = 0
	b.logical = 0
	b.remain = 0
}
