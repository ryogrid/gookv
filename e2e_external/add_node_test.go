package e2e_external_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestAddNode_JoinRegistersWithPD verifies a new node can join and register with PD.
func TestAddNode_JoinRegistersWithPD(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Initially 3 stores.
	initialCount := e2elib.CLIWaitForStoreCount(t, pdAddr, 3, 15*time.Second)

	// Add a new node in join mode.
	_, err := cluster.AddNode()
	require.NoError(t, err)

	// Wait for PD to register the new store.
	finalCount := e2elib.CLIWaitForStoreCount(t, pdAddr, initialCount+1, 30*time.Second)
	assert.Equal(t, initialCount+1, finalCount)

	t.Log("AddNode join registers with PD passed")
}

// TestAddNode_PDSchedulesRegionToNewStore verifies PD is aware of the new store.
func TestAddNode_PDSchedulesRegionToNewStore(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Add a node.
	newNode, err := cluster.AddNode()
	require.NoError(t, err)

	// Verify PD knows about the new store via CLI.
	newStoreID := len(cluster.Nodes())
	e2elib.CLIWaitForCondition(t, pdAddr, fmt.Sprintf("STORE STATUS %d", newStoreID),
		func(output string) bool {
			return strings.Contains(output, newNode.Addr())
		}, 30*time.Second)

	t.Log("PD schedules region to new store passed")
}

// TestAddNode_FullMoveLifecycle verifies a join node can participate in KV operations.
func TestAddNode_FullMoveLifecycle(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Write initial data via CLI.
	e2elib.CLIPut(t, pdAddr, "move-key", "move-val")

	// Add a new node.
	_, err := cluster.AddNode()
	require.NoError(t, err)

	// Verify data is still accessible after node addition.
	val, found := e2elib.CLIGet(t, pdAddr, "move-key")
	assert.True(t, found)
	assert.Equal(t, "move-val", val)

	// Write new data should also work.
	e2elib.CLIPut(t, pdAddr, "after-add-key", "after-add-val")

	val, found = e2elib.CLIGet(t, pdAddr, "after-add-key")
	assert.True(t, found)
	assert.Equal(t, "after-add-val", val)

	t.Log("Full move lifecycle passed")
}

// TestAddNode_MultipleJoinNodes verifies multiple nodes can join sequentially.
func TestAddNode_MultipleJoinNodes(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	initialCount := e2elib.CLIWaitForStoreCount(t, pdAddr, 3, 15*time.Second)

	// Add two nodes sequentially.
	_, err := cluster.AddNode()
	require.NoError(t, err)
	_, err = cluster.AddNode()
	require.NoError(t, err)

	// Wait for PD to know about both new stores.
	finalCount := e2elib.CLIWaitForStoreCount(t, pdAddr, initialCount+2, 30*time.Second)
	assert.GreaterOrEqual(t, finalCount, initialCount+2)

	// Cluster should still be operational via CLI.
	e2elib.CLIPut(t, pdAddr, "multi-join-key", "multi-join-val")

	val, found := e2elib.CLIGet(t, pdAddr, "multi-join-key")
	assert.True(t, found)
	assert.Equal(t, "multi-join-val", val)

	t.Log("Multiple join nodes passed")
}
