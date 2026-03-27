package e2e_external_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestAddNode_JoinRegistersWithPD verifies a new node can join and register with PD.
func TestAddNode_JoinRegistersWithPD(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	// Initially 3 stores.
	stores, err := pdClient.GetAllStores(ctx)
	require.NoError(t, err)
	initialCount := len(stores)

	// Add a new node in join mode.
	_, err = cluster.AddNode()
	require.NoError(t, err)

	// Wait for PD to register the new store.
	finalCount := e2elib.WaitForStoreCount(t, pdClient, initialCount+1, 30*time.Second)
	assert.Equal(t, initialCount+1, finalCount)

	t.Log("AddNode join registers with PD passed")
}

// TestAddNode_PDSchedulesRegionToNewStore verifies PD is aware of the new store.
func TestAddNode_PDSchedulesRegionToNewStore(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	// Add a node.
	newNode, err := cluster.AddNode()
	require.NoError(t, err)

	// Verify PD knows about the new store.
	e2elib.WaitForCondition(t, 30*time.Second, "new store visible in PD", func() bool {
		store, err := pdClient.GetStore(ctx, uint64(len(cluster.Nodes())))
		return err == nil && store != nil && store.GetAddress() == newNode.Addr()
	})

	t.Log("PD schedules region to new store passed")
}

// TestAddNode_FullMoveLifecycle verifies a join node can participate in KV operations.
func TestAddNode_FullMoveLifecycle(t *testing.T) {
	cluster := newClusterWithLeader(t)
	rawKV := cluster.RawKV()
	ctx := context.Background()

	// Write initial data.
	err := rawKV.Put(ctx, []byte("move-key"), []byte("move-val"))
	require.NoError(t, err)

	// Add a new node.
	_, err = cluster.AddNode()
	require.NoError(t, err)

	// Verify data is still accessible after node addition.
	val, notFound, err := rawKV.Get(ctx, []byte("move-key"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("move-val"), val)

	// Write new data should also work.
	err = rawKV.Put(ctx, []byte("after-add-key"), []byte("after-add-val"))
	require.NoError(t, err)

	val, notFound, err = rawKV.Get(ctx, []byte("after-add-key"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("after-add-val"), val)

	t.Log("Full move lifecycle passed")
}

// TestAddNode_MultipleJoinNodes verifies multiple nodes can join sequentially.
func TestAddNode_MultipleJoinNodes(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()
	ctx := context.Background()

	stores, err := pdClient.GetAllStores(ctx)
	require.NoError(t, err)
	initialCount := len(stores)

	// Add two nodes sequentially.
	_, err = cluster.AddNode()
	require.NoError(t, err)
	_, err = cluster.AddNode()
	require.NoError(t, err)

	// Wait for PD to know about both new stores.
	finalCount := e2elib.WaitForStoreCount(t, pdClient, initialCount+2, 30*time.Second)
	assert.GreaterOrEqual(t, finalCount, initialCount+2)

	// Cluster should still be operational.
	rawKV := cluster.RawKV()
	err = rawKV.Put(ctx, []byte("multi-join-key"), []byte("multi-join-val"))
	require.NoError(t, err)

	val, notFound, err := rawKV.Get(ctx, []byte("multi-join-key"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("multi-join-val"), val)

	t.Log("Multiple join nodes passed")
}
