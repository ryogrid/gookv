package e2e_external_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestPDClusterStoreAndRegionHeartbeat verifies the heartbeat loop between
// gookv-server nodes and PD. The servers automatically send heartbeats.
func TestPDClusterStoreAndRegionHeartbeat(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	// Wait for all stores to be registered via heartbeats.
	storeCount := e2elib.WaitForStoreCount(t, pdClient, 3, 30*time.Second)
	assert.Equal(t, 3, storeCount)

	// Verify each store has a correct address.
	for i := 1; i <= 3; i++ {
		store, err := pdClient.GetStore(ctx, uint64(i))
		require.NoError(t, err)
		require.NotNil(t, store)
		assert.NotEmpty(t, store.GetAddress(), "store %d should have an address", i)
	}

	// Verify region heartbeat: PD should know about region 1.
	region, leader, err := pdClient.GetRegion(ctx, []byte(""))
	require.NoError(t, err)
	require.NotNil(t, region)
	assert.Equal(t, uint64(1), region.GetId(), "region 1 should be registered")
	// Leader should be set after heartbeats.
	assert.NotNil(t, leader, "region should have a leader after heartbeats")

	t.Log("PD cluster store and region heartbeat passed")
}

// TestPDClusterTSOForTransactions tests using PD TSO for transaction timestamps.
func TestPDClusterTSOForTransactions(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	// Allocate 100 timestamps and verify monotonicity.
	var prevTS uint64
	for i := 0; i < 100; i++ {
		ts, err := pdClient.GetTS(ctx)
		require.NoError(t, err)
		currentTS := ts.ToUint64()
		assert.Greater(t, currentTS, prevTS, "TSO should be strictly increasing (iter %d)", i)
		prevTS = currentTS
	}

	t.Log("PD cluster TSO for transactions passed")
}

// TestPDClusterGCSafePoint tests GC safe point management via PD.
func TestPDClusterGCSafePoint(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	// Initial GC safe point should be 0.
	sp, err := pdClient.GetGCSafePoint(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(0), sp, "initial GC safe point should be 0")

	// Update GC safe point.
	newSP, err := pdClient.UpdateGCSafePoint(ctx, 1000)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), newSP)

	// Verify it was updated.
	sp, err = pdClient.GetGCSafePoint(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), sp)

	// GC safe point should not go backwards.
	newSP, err = pdClient.UpdateGCSafePoint(ctx, 500)
	require.NoError(t, err)
	// PD should keep the higher value.
	sp, err = pdClient.GetGCSafePoint(ctx)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), sp, "GC safe point should not go backwards")

	t.Log("PD cluster GC safe point passed")
}
