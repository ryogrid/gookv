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

// TestRestartDataSurvives writes data, restarts a follower node, and verifies
// all data is still readable. This confirms Raft log replay works on restart.
func TestRestartDataSurvives(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Write 10 keys via CLI.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("restart-key-%02d", i)
		val := fmt.Sprintf("restart-val-%02d", i)
		e2elib.CLIPut(t, pdAddr, key, val)
	}

	// Restart node 1 (a follower).
	require.NoError(t, cluster.RestartNode(1))
	require.NoError(t, cluster.Node(1).WaitForReady(30*time.Second))

	// Reset client to reconnect after restart.
	cluster.ResetClient()

	// Wait for cluster to stabilize (poll via CLI, which reconnects each invocation).
	e2elib.CLIWaitForCondition(t, pdAddr, "GET restart-key-00",
		func(output string) bool {
			return !strings.Contains(output, "(not found)") && !strings.Contains(output, "error")
		}, 30*time.Second)

	// Verify all 10 keys survived the restart via CLI.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("restart-key-%02d", i)
		expectedVal := fmt.Sprintf("restart-val-%02d", i)
		val, found := e2elib.CLIGet(t, pdAddr, key)
		assert.True(t, found, "key %s should exist after restart", key)
		assert.Equal(t, expectedVal, val)
	}

	t.Log("Restart data survives passed")
}

// TestRestartLeaderFailoverAndReplay stops the leader, writes new data on the
// new leader, restarts the old leader, and verifies all data (old + new) is
// readable. This confirms Raft log replay catches up a restarted node.
func TestRestartLeaderFailoverAndReplay(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Write 10 keys on the original leader via CLI.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("failover-key-%02d", i)
		val := fmt.Sprintf("failover-val-%02d", i)
		e2elib.CLIPut(t, pdAddr, key, val)
	}

	// Find and stop the leader via CLI.
	leaderStoreID := e2elib.CLIWaitForRegionLeader(t, pdAddr, "", 30*time.Second)
	leaderIdx := int(leaderStoreID) - 1
	require.NoError(t, cluster.StopNode(leaderIdx))

	// Reset client after stopping leader.
	cluster.ResetClient()

	// Wait for new leader and write 10 more keys via CLI (poll until PUT succeeds).
	e2elib.CLIWaitForCondition(t, pdAddr, "PUT failover-new-00 new-val-00",
		func(output string) bool {
			return strings.Contains(output, "OK")
		}, 30*time.Second)

	for i := 1; i < 10; i++ {
		key := fmt.Sprintf("failover-new-%02d", i)
		val := fmt.Sprintf("new-val-%02d", i)
		e2elib.CLIWaitForCondition(t, pdAddr, fmt.Sprintf("PUT %s %s", key, val),
			func(output string) bool {
				return strings.Contains(output, "OK")
			}, 15*time.Second)
	}

	// Restart the old leader.
	require.NoError(t, cluster.RestartNode(leaderIdx))
	require.NoError(t, cluster.Node(leaderIdx).WaitForReady(30*time.Second))

	// Reset client and wait for cluster to stabilize.
	cluster.ResetClient()
	e2elib.CLIWaitForCondition(t, pdAddr, "GET failover-key-00",
		func(output string) bool {
			return !strings.Contains(output, "(not found)") && !strings.Contains(output, "error")
		}, 30*time.Second)

	// Verify all 20 keys (10 old + 10 new) via CLI.
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("failover-key-%02d", i)
		expectedVal := fmt.Sprintf("failover-val-%02d", i)
		val, found := e2elib.CLIGet(t, pdAddr, key)
		assert.True(t, found, "old key %s should exist", key)
		assert.Equal(t, expectedVal, val, "old key %s", key)
	}
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("failover-new-%02d", i)
		expectedVal := fmt.Sprintf("new-val-%02d", i)
		val, found := e2elib.CLIGet(t, pdAddr, key)
		assert.True(t, found, "new key %s should exist after replay", key)
		assert.Equal(t, expectedVal, val, "new key %s", key)
	}

	t.Log("Restart leader failover and replay passed")
}

// TestRestartAllNodesDataSurvives stops all 3 nodes, restarts them all,
// and verifies data integrity. This is the most demanding restart test,
// confirming the full cluster can recover from a cold start.
func TestRestartAllNodesDataSurvives(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Write 20 keys via CLI.
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("allrestart-key-%02d", i)
		val := fmt.Sprintf("allrestart-val-%02d", i)
		e2elib.CLIPut(t, pdAddr, key, val)
	}

	// Stop all 3 nodes.
	for i := 2; i >= 0; i-- {
		require.NoError(t, cluster.StopNode(i))
	}

	// Restart all 3 nodes.
	for i := 0; i < 3; i++ {
		require.NoError(t, cluster.RestartNode(i))
		require.NoError(t, cluster.Node(i).WaitForReady(30*time.Second))
	}

	// Reset client and wait for leader election (poll via CLI).
	cluster.ResetClient()
	e2elib.CLIWaitForCondition(t, pdAddr, "GET allrestart-key-00",
		func(output string) bool {
			return !strings.Contains(output, "(not found)") && !strings.Contains(output, "error")
		}, 60*time.Second)

	// Verify all 20 keys survived full cluster restart via CLI.
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("allrestart-key-%02d", i)
		expectedVal := fmt.Sprintf("allrestart-val-%02d", i)
		val, found := e2elib.CLIGet(t, pdAddr, key)
		assert.True(t, found, "key %s should survive full restart", key)
		assert.Equal(t, expectedVal, val, "key %s", key)
	}

	t.Log("Restart all nodes data survives passed")
}
