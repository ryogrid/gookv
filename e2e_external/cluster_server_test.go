package e2e_external_test

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestClusterServerLeaderElection verifies that a 3-node cluster elects a leader.
func TestClusterServerLeaderElection(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// PD should know about a leader for region containing "".
	leaderStoreID := e2elib.CLIWaitForRegionLeader(t, pdAddr, "", 30*time.Second)
	assert.NotZero(t, leaderStoreID, "should have a leader")

	t.Log("Cluster leader election passed")
}

// TestClusterServerKvOperations tests prewrite → commit → get on the cluster.
func TestClusterServerKvOperations(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	e2elib.CLIPut(t, pdAddr, "kv-op-key", "kv-op-val")

	val, found := e2elib.CLIGet(t, pdAddr, "kv-op-key")
	assert.True(t, found)
	assert.Equal(t, "kv-op-val", val)

	t.Log("Cluster KV operations passed")
}

// TestClusterServerCrossNodeReplication writes on leader and verifies data is replicated.
func TestClusterServerCrossNodeReplication(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Write via CLI (auto-routes to leader via PD).
	e2elib.CLIPut(t, pdAddr, "repl-key", "repl-val")

	// Read back via CLI.
	val, found := e2elib.CLIGet(t, pdAddr, "repl-key")
	assert.True(t, found)
	assert.Equal(t, "repl-val", val)

	// Also verify via CLI on each node.
	replicatedCount := 0
	for i := 0; i < 3; i++ {
		e2elib.WaitForCondition(t, 15*time.Second, "replication to node", func() bool {
			nodeVal, nodeFound := e2elib.CLINodeGet(t, cluster.Node(i).Addr(), "repl-key")
			if nodeFound {
				assert.Equal(t, "repl-val", nodeVal, "node %d should have replicated value", i)
				replicatedCount++
				return true
			}
			return false
		})
	}
	assert.GreaterOrEqual(t, replicatedCount, 1, "at least 1 node should have the replicated value")

	t.Log("Cross-node replication passed")
}

// TestClusterServerNodeFailure verifies the cluster survives minority node failure.
func TestClusterServerNodeFailure(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Write initial data via CLI.
	e2elib.CLIPut(t, pdAddr, "survive-key", "survive-val")

	// Stop one node (minority failure in a 3-node cluster).
	require.NoError(t, cluster.StopNode(2))

	// Cluster should still be operational (2 of 3 nodes = majority).
	e2elib.CLIWaitForCondition(t, pdAddr, "PUT after-fail-key after-fail-val",
		func(output string) bool {
			return strings.Contains(output, "OK")
		}, 15*time.Second)

	// Read should work.
	val, found := e2elib.CLIGet(t, pdAddr, "survive-key")
	assert.True(t, found)
	assert.Equal(t, "survive-val", val)

	t.Log("Cluster survives minority node failure passed")
}

// TestClusterServerLeaderFailover verifies new leader election after old leader stops.
func TestClusterServerLeaderFailover(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Write initial data via CLI.
	e2elib.CLIPut(t, pdAddr, "failover-key", "failover-val")

	// Find current leader via CLI.
	leaderStoreID := e2elib.CLIWaitForRegionLeader(t, pdAddr, "", 30*time.Second)

	// Stop the leader node (store IDs are 1-indexed, node indices are 0-indexed).
	leaderIdx := int(leaderStoreID) - 1
	require.NoError(t, cluster.StopNode(leaderIdx))

	// Wait for new leader and verify operations work via CLI.
	e2elib.CLIWaitForCondition(t, pdAddr, "PUT new-leader-key new-leader-val",
		func(output string) bool {
			return strings.Contains(output, "OK")
		}, 30*time.Second)

	// Read old data should still work.
	val, found := e2elib.CLIGet(t, pdAddr, "failover-key")
	assert.True(t, found)
	assert.Equal(t, "failover-val", val)

	t.Log("Leader failover passed")
}
