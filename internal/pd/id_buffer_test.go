package pd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIDBuffer_AllocateFromBuffer verifies that pre-filled buffer returns
// sequential IDs without additional Raft proposals.
func TestIDBuffer_AllocateFromBuffer(t *testing.T) {
	peers, cleanup := createTestPDRaftCluster(t, 3)
	defer cleanup()

	leaderIdx := waitForLeader(t, peers, 3*time.Second)
	require.NotEqual(t, -1, leaderIdx, "no leader elected")

	// Wire applyFunc on all peers via a test PDServer.
	srv := newTestPDServerForBuffer(t, peers)
	_ = srv

	leader := peers[leaderIdx]
	buf := NewIDBuffer(leader)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Allocate several IDs.
	var prevID uint64
	for i := 0; i < 10; i++ {
		id, err := buf.Alloc(ctx)
		require.NoError(t, err, "allocation %d failed", i)
		require.NotZero(t, id, "allocation %d returned zero", i)

		if i > 0 {
			// IDs should be sequential within the batch.
			assert.Equal(t, prevID+1, id,
				"ID %d not sequential: prev=%d cur=%d", i, prevID, id)
		}
		prevID = id
	}
}

// TestIDBuffer_RefillOnDepletion verifies that when the buffer is exhausted,
// it automatically refills via a new Raft proposal and continues returning
// unique IDs.
func TestIDBuffer_RefillOnDepletion(t *testing.T) {
	peers, cleanup := createTestPDRaftCluster(t, 3)
	defer cleanup()

	leaderIdx := waitForLeader(t, peers, 3*time.Second)
	require.NotEqual(t, -1, leaderIdx, "no leader elected")

	srv := newTestPDServerForBuffer(t, peers)
	_ = srv

	leader := peers[leaderIdx]
	buf := NewIDBuffer(leader)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Exhaust the first batch entirely.
	seen := make(map[uint64]bool)
	for i := 0; i < idBatchSize; i++ {
		id, err := buf.Alloc(ctx)
		require.NoError(t, err, "allocation %d failed", i)
		assert.False(t, seen[id], "duplicate ID %d at index %d", id, i)
		seen[id] = true
	}

	// Buffer should now be depleted. Next call triggers refill.
	id, err := buf.Alloc(ctx)
	require.NoError(t, err)
	assert.False(t, seen[id], "post-refill ID %d is a duplicate", id)
	assert.NotZero(t, id)
}
