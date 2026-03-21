package pd

import (
	"context"
	"encoding/binary"
	"fmt"
	"sync"
)

const idBatchSize = 100

// IDBuffer pre-allocates ID ranges via Raft and serves subsequent requests
// from a local buffer. This amortizes the cost of Raft consensus over many
// ID allocations. On leader change, the buffer must be reset so the new
// leader proposes a fresh batch.
type IDBuffer struct {
	mu       sync.Mutex
	nextID   uint64
	endID    uint64 // exclusive upper bound
	raftPeer *PDRaftPeer
}

// NewIDBuffer creates an IDBuffer backed by the given Raft peer.
func NewIDBuffer(raftPeer *PDRaftPeer) *IDBuffer {
	return &IDBuffer{
		raftPeer: raftPeer,
	}
}

// Alloc allocates a single unique ID from the buffer. If the buffer is
// depleted, it proposes a new CmdIDAlloc batch via Raft.
func (b *IDBuffer) Alloc(ctx context.Context) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.nextID >= b.endID {
		if err := b.refill(ctx); err != nil {
			return 0, err
		}
	}

	id := b.nextID
	b.nextID++
	return id, nil
}

// refill proposes a CmdIDAlloc with a batch size via Raft. The result is the
// last allocated ID; the buffer range is [lastID - batchSize + 1, lastID + 1).
// Must be called with b.mu held.
func (b *IDBuffer) refill(ctx context.Context) error {
	cmd := PDCommand{
		Type:        CmdIDAlloc,
		IDBatchSize: idBatchSize,
	}

	result, err := b.raftPeer.ProposeAndWait(ctx, cmd)
	if err != nil {
		return fmt.Errorf("id buffer: propose: %w", err)
	}

	if len(result) < 8 {
		return fmt.Errorf("id buffer: invalid result length %d", len(result))
	}

	lastID := binary.BigEndian.Uint64(result)
	b.nextID = lastID - uint64(idBatchSize) + 1
	b.endID = lastID + 1

	return nil
}

// Reset clears the buffer. Must be called on leader change so the new leader
// proposes a fresh batch.
func (b *IDBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextID = 0
	b.endID = 0
}
