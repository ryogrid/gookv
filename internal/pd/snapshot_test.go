package pd

import (
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPDSnapshot_GenerateAndApply(t *testing.T) {
	srv := newTestPDServer(t)

	// Populate state: 2 stores, 3 regions with leaders, store stats, GC safe point, 1 pending move.
	srv.meta.PutStore(&metapb.Store{Id: 1, Address: "127.0.0.1:20160"})
	srv.meta.PutStore(&metapb.Store{Id: 2, Address: "127.0.0.1:20161"})

	srv.meta.PutRegion(&metapb.Region{
		Id:       1,
		StartKey: []byte("a"),
		EndKey:   []byte("m"),
		Peers: []*metapb.Peer{
			{Id: 10, StoreId: 1},
			{Id: 11, StoreId: 2},
		},
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
	}, &metapb.Peer{Id: 10, StoreId: 1})

	srv.meta.PutRegion(&metapb.Region{
		Id:       2,
		StartKey: []byte("m"),
		EndKey:   []byte("z"),
		Peers: []*metapb.Peer{
			{Id: 20, StoreId: 1},
			{Id: 21, StoreId: 2},
		},
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
	}, &metapb.Peer{Id: 20, StoreId: 1})

	srv.meta.PutRegion(&metapb.Region{
		Id:       3,
		StartKey: nil,
		EndKey:   []byte("a"),
		Peers: []*metapb.Peer{
			{Id: 30, StoreId: 2},
		},
	}, &metapb.Peer{Id: 30, StoreId: 2})

	srv.meta.UpdateStoreStats(1, &pdpb.StoreStats{
		StoreId:   1,
		Available: 1000,
		Capacity:  2000,
	})

	srv.meta.SetBootstrapped(true)
	srv.gcMgr.UpdateSafePoint(500)
	srv.moveTracker.StartMove(1, &metapb.Peer{Id: 10, StoreId: 1}, 2)

	// Allocate some IDs and TSO to set state.
	for i := 0; i < 5; i++ {
		srv.idAlloc.Alloc()
	}
	srv.tso.Allocate(10)

	// Generate snapshot.
	data, err := srv.GenerateSnapshot()
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Create fresh server and apply snapshot.
	srv2 := newTestPDServer(t)
	err = srv2.ApplySnapshot(data)
	require.NoError(t, err)

	// Verify all fields match.
	assert.True(t, srv2.meta.IsBootstrapped())

	store1 := srv2.meta.GetStore(1)
	require.NotNil(t, store1)
	assert.Equal(t, "127.0.0.1:20160", store1.GetAddress())

	store2 := srv2.meta.GetStore(2)
	require.NotNil(t, store2)
	assert.Equal(t, "127.0.0.1:20161", store2.GetAddress())

	region1, leader1 := srv2.meta.GetRegionByID(1)
	require.NotNil(t, region1)
	assert.Equal(t, uint64(1), region1.GetId())
	assert.Len(t, region1.GetPeers(), 2)
	require.NotNil(t, leader1)
	assert.Equal(t, uint64(1), leader1.GetStoreId())

	region2, leader2 := srv2.meta.GetRegionByID(2)
	require.NotNil(t, region2)
	require.NotNil(t, leader2)

	region3, leader3 := srv2.meta.GetRegionByID(3)
	require.NotNil(t, region3)
	require.NotNil(t, leader3)
	assert.Equal(t, uint64(2), leader3.GetStoreId())

	assert.Equal(t, uint64(500), srv2.gcMgr.GetSafePoint())
	assert.True(t, srv2.moveTracker.HasPendingMove(1))

	// ID allocator state should be preserved.
	srv2.idAlloc.mu.Lock()
	nextID2 := srv2.idAlloc.nextID
	srv2.idAlloc.mu.Unlock()
	srv.idAlloc.mu.Lock()
	nextID1 := srv.idAlloc.nextID
	srv.idAlloc.mu.Unlock()
	assert.Equal(t, nextID1, nextID2)

	// TSO state should be preserved.
	srv2.tso.mu.Lock()
	phys2 := srv2.tso.physical
	log2 := srv2.tso.logical
	srv2.tso.mu.Unlock()
	srv.tso.mu.Lock()
	phys1 := srv.tso.physical
	log1 := srv.tso.logical
	srv.tso.mu.Unlock()
	assert.Equal(t, phys1, phys2)
	assert.Equal(t, log1, log2)
}

func TestPDSnapshot_EmptyState(t *testing.T) {
	srv := newTestPDServer(t)

	// Generate snapshot of empty PD.
	data, err := srv.GenerateSnapshot()
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Apply to fresh server.
	srv2 := newTestPDServer(t)
	err = srv2.ApplySnapshot(data)
	require.NoError(t, err)

	assert.False(t, srv2.meta.IsBootstrapped())
	assert.Empty(t, srv2.meta.GetAllStores())
	assert.Equal(t, uint64(0), srv2.gcMgr.GetSafePoint())
	assert.Equal(t, 0, srv2.moveTracker.ActiveMoveCount())
}

func TestPDSnapshot_TSOState(t *testing.T) {
	srv := newTestPDServer(t)

	// Allocate 100 timestamps.
	var lastTS *pdpb.Timestamp
	for i := 0; i < 100; i++ {
		ts, err := srv.tso.Allocate(1)
		require.NoError(t, err)
		lastTS = ts
	}

	// Snapshot.
	data, err := srv.GenerateSnapshot()
	require.NoError(t, err)

	// Apply to fresh server.
	srv2 := newTestPDServer(t)
	err = srv2.ApplySnapshot(data)
	require.NoError(t, err)

	// Allocate 1 more timestamp on the restored server.
	// Need a small sleep so physical clock advances if needed.
	time.Sleep(2 * time.Millisecond)
	newTS, err := srv2.tso.Allocate(1)
	require.NoError(t, err)

	// New timestamp should be >= the last timestamp from the original server.
	lastUint := uint64(lastTS.Physical)<<18 | uint64(lastTS.Logical)
	newUint := uint64(newTS.Physical)<<18 | uint64(newTS.Logical)
	assert.True(t, newUint > lastUint,
		"new TS (%d) should be > last TS (%d)", newUint, lastUint)
}

func TestPDSnapshot_IDAllocState(t *testing.T) {
	srv := newTestPDServer(t)

	// Allocate 10 IDs.
	var lastID uint64
	for i := 0; i < 10; i++ {
		lastID = srv.idAlloc.Alloc()
	}

	// Snapshot.
	data, err := srv.GenerateSnapshot()
	require.NoError(t, err)

	// Apply to fresh server.
	srv2 := newTestPDServer(t)
	err = srv2.ApplySnapshot(data)
	require.NoError(t, err)

	// Allocate 1 more.
	newID := srv2.idAlloc.Alloc()
	assert.True(t, newID > lastID,
		"new ID (%d) should be > last ID (%d)", newID, lastID)
}

func TestPDSnapshot_LargeState(t *testing.T) {
	srv := newTestPDServer(t)

	// Register 1000 regions with leaders.
	for i := uint64(1); i <= 1000; i++ {
		srv.meta.PutRegion(&metapb.Region{
			Id: i,
			Peers: []*metapb.Peer{
				{Id: i * 10, StoreId: 1},
			},
		}, &metapb.Peer{Id: i * 10, StoreId: 1})
	}

	// Snapshot and apply.
	data, err := srv.GenerateSnapshot()
	require.NoError(t, err)

	srv2 := newTestPDServer(t)
	err = srv2.ApplySnapshot(data)
	require.NoError(t, err)

	// Verify all 1000 regions are preserved.
	allRegions := srv2.meta.GetAllRegions()
	assert.Len(t, allRegions, 1000)
	for i := uint64(1); i <= 1000; i++ {
		region, leader := srv2.meta.GetRegionByID(i)
		assert.NotNil(t, region, "region %d should exist", i)
		assert.NotNil(t, leader, "leader for region %d should exist", i)
		assert.Equal(t, i*10, leader.GetId())
	}
}
