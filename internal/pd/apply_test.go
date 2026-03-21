package pd

import (
	"encoding/binary"
	"encoding/json"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newTestPDServer creates a minimal PDServer for apply tests.
// It does not call Start(), so no network listener or background goroutines run.
func newTestPDServer(t *testing.T) *PDServer {
	t.Helper()
	cfg := DefaultPDServerConfig()
	cfg.ListenAddr = "127.0.0.1:0"
	srv, err := NewPDServer(cfg)
	require.NoError(t, err)
	t.Cleanup(func() {
		srv.cancel()
	})
	return srv
}

func TestApplyCommand_SetBootstrapped(t *testing.T) {
	srv := newTestPDServer(t)

	assert.False(t, srv.meta.IsBootstrapped())

	boolTrue := true
	_, err := srv.applyCommand(PDCommand{
		Type:         CmdSetBootstrapped,
		Bootstrapped: &boolTrue,
	})
	require.NoError(t, err)

	assert.True(t, srv.meta.IsBootstrapped())
}

func TestApplyCommand_PutStore(t *testing.T) {
	srv := newTestPDServer(t)

	store := &metapb.Store{
		Id:      1,
		Address: "127.0.0.1:20160",
	}
	_, err := srv.applyCommand(PDCommand{
		Type:  CmdPutStore,
		Store: store,
	})
	require.NoError(t, err)

	got := srv.meta.GetStore(1)
	require.NotNil(t, got)
	assert.Equal(t, uint64(1), got.GetId())
	assert.Equal(t, "127.0.0.1:20160", got.GetAddress())
}

func TestApplyCommand_PutRegion(t *testing.T) {
	srv := newTestPDServer(t)

	region := &metapb.Region{
		Id: 10,
		Peers: []*metapb.Peer{
			{Id: 100, StoreId: 1},
			{Id: 101, StoreId: 2},
			{Id: 102, StoreId: 3},
		},
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
	}
	leader := &metapb.Peer{Id: 100, StoreId: 1}

	_, err := srv.applyCommand(PDCommand{
		Type:   CmdPutRegion,
		Region: region,
		Leader: leader,
	})
	require.NoError(t, err)

	gotRegion, gotLeader := srv.meta.GetRegionByID(10)
	require.NotNil(t, gotRegion)
	assert.Equal(t, uint64(10), gotRegion.GetId())
	assert.Len(t, gotRegion.GetPeers(), 3)
	require.NotNil(t, gotLeader)
	assert.Equal(t, uint64(1), gotLeader.GetStoreId())
}

func TestApplyCommand_UpdateStoreStats(t *testing.T) {
	srv := newTestPDServer(t)

	// Register a store first.
	srv.meta.PutStore(&metapb.Store{Id: 1, Address: "127.0.0.1:20160"})

	stats := &pdpb.StoreStats{
		StoreId:            1,
		Available:          1000,
		Capacity:           2000,
		RegionCount:        5,
		SendingSnapCount:   0,
		ReceivingSnapCount: 0,
	}
	_, err := srv.applyCommand(PDCommand{
		Type:       CmdUpdateStoreStats,
		StoreID:    1,
		StoreStats: stats,
	})
	require.NoError(t, err)

	// Verify via the storeStats map directly (MetadataStore internal).
	srv.meta.mu.RLock()
	gotStats := srv.meta.storeStats[1]
	srv.meta.mu.RUnlock()
	require.NotNil(t, gotStats)
	assert.Equal(t, uint64(1000), gotStats.GetAvailable())
	assert.Equal(t, uint64(2000), gotStats.GetCapacity())
}

func TestApplyCommand_SetStoreState(t *testing.T) {
	srv := newTestPDServer(t)

	// Register a store first.
	srv.meta.PutStore(&metapb.Store{Id: 1, Address: "127.0.0.1:20160"})

	tombstone := StoreStateTombstone
	_, err := srv.applyCommand(PDCommand{
		Type:       CmdSetStoreState,
		StoreID:    1,
		StoreState: &tombstone,
	})
	require.NoError(t, err)

	assert.Equal(t, StoreStateTombstone, srv.meta.GetStoreState(1))
}

func TestApplyCommand_TSOAllocate(t *testing.T) {
	srv := newTestPDServer(t)

	result, err := srv.applyCommand(PDCommand{
		Type:         CmdTSOAllocate,
		TSOBatchSize: 100,
	})
	require.NoError(t, err)
	require.NotNil(t, result)

	var ts pdpb.Timestamp
	err = json.Unmarshal(result, &ts)
	require.NoError(t, err)
	assert.True(t, ts.Physical > 0, "physical timestamp should be > 0")
}

func TestApplyCommand_IDAlloc(t *testing.T) {
	srv := newTestPDServer(t)

	result1, err := srv.applyCommand(PDCommand{
		Type: CmdIDAlloc,
	})
	require.NoError(t, err)
	require.Len(t, result1, 8)

	id1 := binary.BigEndian.Uint64(result1)
	assert.True(t, id1 > 0, "allocated ID should be > 0")

	// Second allocation should return a higher value.
	result2, err := srv.applyCommand(PDCommand{
		Type: CmdIDAlloc,
	})
	require.NoError(t, err)

	id2 := binary.BigEndian.Uint64(result2)
	assert.True(t, id2 > id1, "second ID (%d) should be > first ID (%d)", id2, id1)
}

func TestApplyCommand_UpdateGCSafePoint(t *testing.T) {
	srv := newTestPDServer(t)

	result, err := srv.applyCommand(PDCommand{
		Type:        CmdUpdateGCSafePoint,
		GCSafePoint: 1000,
	})
	require.NoError(t, err)
	require.Len(t, result, 8)

	sp := binary.BigEndian.Uint64(result)
	assert.Equal(t, uint64(1000), sp)
	assert.Equal(t, uint64(1000), srv.gcMgr.GetSafePoint())
}

func TestApplyCommand_StartMove(t *testing.T) {
	srv := newTestPDServer(t)

	// Register a region so move makes semantic sense.
	srv.meta.PutRegion(&metapb.Region{
		Id: 10,
		Peers: []*metapb.Peer{
			{Id: 100, StoreId: 1},
			{Id: 101, StoreId: 2},
		},
	}, &metapb.Peer{Id: 100, StoreId: 1})

	sourcePeer := &metapb.Peer{Id: 100, StoreId: 1}
	_, err := srv.applyCommand(PDCommand{
		Type:              CmdStartMove,
		MoveRegionID:      10,
		MoveSourcePeer:    sourcePeer,
		MoveTargetStoreID: 3,
	})
	require.NoError(t, err)

	assert.True(t, srv.moveTracker.HasPendingMove(10))
}

func TestApplyCommand_AdvanceMove(t *testing.T) {
	srv := newTestPDServer(t)

	// Set up a pending move.
	sourcePeer := &metapb.Peer{Id: 100, StoreId: 1}
	srv.moveTracker.StartMove(10, sourcePeer, 3)

	// Advance with a region that now has the target peer (simulates AddPeer complete).
	region := &metapb.Region{
		Id: 10,
		Peers: []*metapb.Peer{
			{Id: 100, StoreId: 1},
			{Id: 101, StoreId: 2},
			{Id: 102, StoreId: 3}, // target added
		},
	}
	leader := &metapb.Peer{Id: 101, StoreId: 2} // leader is not on source

	result, err := srv.applyCommand(PDCommand{
		Type:          CmdAdvanceMove,
		MoveRegionID:  10,
		AdvanceRegion: region,
		AdvanceLeader: leader,
	})
	require.NoError(t, err)
	assert.Nil(t, result, "AdvanceMove should return nil result (ScheduleCommand is discarded)")
}

func TestApplyCommand_CleanupStaleMove(t *testing.T) {
	srv := newTestPDServer(t)

	// Start a move then make it stale by using a very short timeout.
	sourcePeer := &metapb.Peer{Id: 100, StoreId: 1}
	srv.moveTracker.StartMove(10, sourcePeer, 3)
	require.True(t, srv.moveTracker.HasPendingMove(10))

	// Use a 0-duration timeout so any move is considered stale.
	_, err := srv.applyCommand(PDCommand{
		Type:           CmdCleanupStaleMove,
		CleanupTimeout: 0,
	})
	require.NoError(t, err)

	// The move should still exist because CleanupStale uses >, not >=.
	// Use a small sleep and retry with 1ns to ensure cleanup.
	time.Sleep(1 * time.Millisecond)
	_, err = srv.applyCommand(PDCommand{
		Type:           CmdCleanupStaleMove,
		CleanupTimeout: 1 * time.Nanosecond,
	})
	require.NoError(t, err)

	assert.False(t, srv.moveTracker.HasPendingMove(10))
}

func TestApplyCommand_AllTypes(t *testing.T) {
	srv := newTestPDServer(t)

	boolTrue := true
	storeStateUp := StoreStateUp

	// Pre-register a store and region for commands that need them.
	srv.meta.PutStore(&metapb.Store{Id: 1, Address: "127.0.0.1:20160"})
	srv.meta.PutRegion(&metapb.Region{
		Id: 10,
		Peers: []*metapb.Peer{
			{Id: 100, StoreId: 1},
			{Id: 101, StoreId: 2},
		},
	}, &metapb.Peer{Id: 100, StoreId: 1})

	// Start a move for AdvanceMove.
	srv.moveTracker.StartMove(10, &metapb.Peer{Id: 100, StoreId: 1}, 3)

	commands := []PDCommand{
		{Type: CmdSetBootstrapped, Bootstrapped: &boolTrue},
		{Type: CmdPutStore, Store: &metapb.Store{Id: 2, Address: "127.0.0.1:20161"}},
		{Type: CmdPutRegion, Region: &metapb.Region{Id: 20, Peers: []*metapb.Peer{{Id: 200, StoreId: 1}}}, Leader: &metapb.Peer{Id: 200, StoreId: 1}},
		{Type: CmdUpdateStoreStats, StoreID: 1, StoreStats: &pdpb.StoreStats{StoreId: 1, Available: 500}},
		{Type: CmdSetStoreState, StoreID: 1, StoreState: &storeStateUp},
		{Type: CmdTSOAllocate, TSOBatchSize: 10},
		{Type: CmdIDAlloc},
		{Type: CmdUpdateGCSafePoint, GCSafePoint: 500},
		{Type: CmdStartMove, MoveRegionID: 20, MoveSourcePeer: &metapb.Peer{Id: 200, StoreId: 1}, MoveTargetStoreID: 3},
		{Type: CmdAdvanceMove, MoveRegionID: 10, AdvanceRegion: &metapb.Region{
			Id: 10,
			Peers: []*metapb.Peer{
				{Id: 100, StoreId: 1},
				{Id: 101, StoreId: 2},
				{Id: 102, StoreId: 3},
			},
		}, AdvanceLeader: &metapb.Peer{Id: 101, StoreId: 2}},
		{Type: CmdCleanupStaleMove, CleanupTimeout: 10 * time.Minute},
	}

	for i, cmd := range commands {
		_, err := srv.applyCommand(cmd)
		assert.NoError(t, err, "command %d (type=%d) should not error", i, cmd.Type)
	}

	// Verify key side effects.
	assert.True(t, srv.meta.IsBootstrapped())
	assert.NotNil(t, srv.meta.GetStore(2))
	region, leader := srv.meta.GetRegionByID(20)
	assert.NotNil(t, region)
	assert.NotNil(t, leader)
	assert.Equal(t, uint64(500), srv.gcMgr.GetSafePoint())
}
