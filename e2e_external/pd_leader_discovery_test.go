package e2e_external_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestPDStoreRegistration verifies all stores are registered with PD.
func TestPDStoreRegistration(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	// All 3 stores should be registered.
	stores, err := pdClient.GetAllStores(ctx)
	require.NoError(t, err)
	assert.Len(t, stores, 3, "all 3 stores should be registered")

	// Verify each store has a valid address matching a node.
	nodeAddrs := make(map[string]bool)
	for _, node := range cluster.Nodes() {
		nodeAddrs[node.Addr()] = true
	}

	for _, store := range stores {
		assert.True(t, nodeAddrs[store.GetAddress()],
			"store %d address %s should match a node", store.GetId(), store.GetAddress())
	}

	t.Log("PD store registration passed")
}

// TestPDRegionLeaderTracking verifies PD tracks the region leader.
func TestPDRegionLeaderTracking(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()

	// Wait for PD to know the leader.
	leaderStoreID := e2elib.WaitForRegionLeader(t, pdClient, []byte(""), 30*time.Second)
	assert.NotZero(t, leaderStoreID)

	// Verify the leader store exists.
	ctx := context.Background()
	store, err := pdClient.GetStore(ctx, leaderStoreID)
	require.NoError(t, err)
	require.NotNil(t, store)
	assert.NotEmpty(t, store.GetAddress())

	t.Log("PD region leader tracking passed")
}

// TestPDLeaderFailover verifies PD updates to the new leader after the old one stops.
func TestPDLeaderFailover(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	// Get initial leader.
	oldLeaderStoreID := e2elib.WaitForRegionLeader(t, pdClient, []byte(""), 30*time.Second)
	require.NotZero(t, oldLeaderStoreID)

	// Stop the leader node.
	leaderIdx := int(oldLeaderStoreID) - 1
	require.NoError(t, cluster.StopNode(leaderIdx))

	// Wait for PD to report a different leader.
	e2elib.WaitForCondition(t, 30*time.Second, "new leader in PD", func() bool {
		_, leader, err := pdClient.GetRegion(ctx, []byte(""))
		if err != nil || leader == nil {
			return false
		}
		return leader.GetStoreId() != 0 && leader.GetStoreId() != oldLeaderStoreID
	})

	// Verify the new leader is reachable.
	_, newLeader, err := pdClient.GetRegion(ctx, []byte(""))
	require.NoError(t, err)
	require.NotNil(t, newLeader)
	assert.NotEqual(t, oldLeaderStoreID, newLeader.GetStoreId(), "leader should have changed")

	// Verify the new leader's store address is valid.
	store, err := pdClient.GetStore(ctx, newLeader.GetStoreId())
	require.NoError(t, err)
	assert.NotEmpty(t, store.GetAddress())

	t.Log("PD leader failover passed")
}
