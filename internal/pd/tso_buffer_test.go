package pd

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTSOBuffer_AllocateFromBuffer verifies that pre-filled buffer returns
// monotonically increasing timestamps without additional Raft proposals.
func TestTSOBuffer_AllocateFromBuffer(t *testing.T) {
	peers, cleanup := createTestPDRaftCluster(t, 3)
	defer cleanup()

	leaderIdx := waitForLeader(t, peers, 3*time.Second)
	require.NotEqual(t, -1, leaderIdx, "no leader elected")

	// Wire applyFunc on all peers via a test PDServer.
	srv := newTestPDServerForBuffer(t, peers)
	_ = srv

	leader := peers[leaderIdx]
	buf := NewTSOBuffer(leader)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Allocate several timestamps.
	var prevPhysical int64
	var prevLogical int64
	for i := 0; i < 10; i++ {
		ts, err := buf.GetTS(ctx, 1)
		require.NoError(t, err, "allocation %d failed", i)
		require.NotNil(t, ts)

		// Timestamps must be monotonically increasing.
		if i > 0 {
			assert.True(t,
				ts.Physical > prevPhysical || (ts.Physical == prevPhysical && ts.Logical > prevLogical),
				"timestamp %d not monotonically increasing: prev=(%d,%d) cur=(%d,%d)",
				i, prevPhysical, prevLogical, ts.Physical, ts.Logical,
			)
		}
		prevPhysical = ts.Physical
		prevLogical = ts.Logical
	}
}

// TestTSOBuffer_RefillOnDepletion verifies that when the buffer is exhausted,
// it automatically refills via a new Raft proposal.
func TestTSOBuffer_RefillOnDepletion(t *testing.T) {
	peers, cleanup := createTestPDRaftCluster(t, 3)
	defer cleanup()

	leaderIdx := waitForLeader(t, peers, 3*time.Second)
	require.NotEqual(t, -1, leaderIdx, "no leader elected")

	srv := newTestPDServerForBuffer(t, peers)
	_ = srv

	leader := peers[leaderIdx]
	buf := NewTSOBuffer(leader)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Exhaust the entire first batch by requesting all at once.
	ts1, err := buf.GetTS(ctx, tsoBatchSize)
	require.NoError(t, err)
	require.NotNil(t, ts1)

	// Buffer should now be depleted (remain == 0). Next call triggers refill.
	ts2, err := buf.GetTS(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, ts2)

	// ts2 must be greater than ts1 (new batch from Raft).
	assert.True(t,
		ts2.Physical > ts1.Physical || (ts2.Physical == ts1.Physical && ts2.Logical > ts1.Logical),
		"post-refill timestamp not greater: ts1=(%d,%d) ts2=(%d,%d)",
		ts1.Physical, ts1.Logical, ts2.Physical, ts2.Logical,
	)
}

// TestTSOBuffer_LeaderChange verifies that Reset() clears the buffer and the
// next allocation starts a fresh batch via Raft.
func TestTSOBuffer_LeaderChange(t *testing.T) {
	peers, cleanup := createTestPDRaftCluster(t, 3)
	defer cleanup()

	leaderIdx := waitForLeader(t, peers, 3*time.Second)
	require.NotEqual(t, -1, leaderIdx, "no leader elected")

	srv := newTestPDServerForBuffer(t, peers)
	_ = srv

	leader := peers[leaderIdx]
	buf := NewTSOBuffer(leader)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Allocate a few timestamps.
	ts1, err := buf.GetTS(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, ts1)

	// Simulate leader change by resetting the buffer.
	buf.Reset()

	// Next allocation should trigger a new Raft proposal.
	ts2, err := buf.GetTS(ctx, 1)
	require.NoError(t, err)
	require.NotNil(t, ts2)

	// ts2 should be from a fresh batch (physical >= ts1.Physical).
	assert.True(t,
		ts2.Physical > ts1.Physical || (ts2.Physical == ts1.Physical && ts2.Logical > ts1.Logical),
		"post-reset timestamp not greater: ts1=(%d,%d) ts2=(%d,%d)",
		ts1.Physical, ts1.Logical, ts2.Physical, ts2.Logical,
	)
}

// newTestPDServerForBuffer creates a minimal PDServer (single-node, no gRPC)
// and wires its applyCommand as the applyFunc for all peers.
func newTestPDServerForBuffer(t *testing.T, peers []*PDRaftPeer) *PDServer {
	t.Helper()
	cfg := DefaultPDServerConfig()
	srv := &PDServer{
		cfg:         cfg,
		clusterID:   cfg.ClusterID,
		tso:         NewTSOAllocator(cfg.TSOSaveInterval),
		meta:        NewMetadataStore(cfg.ClusterID, cfg.StoreDisconnectDuration, cfg.StoreDownDuration),
		idAlloc:     NewIDAllocator(),
		gcMgr:       NewGCSafePointManager(),
		moveTracker: NewMoveTracker(),
	}
	for _, p := range peers {
		p.SetApplyFunc(srv.applyCommand)
	}
	return srv
}
