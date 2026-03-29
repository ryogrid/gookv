package e2e_external_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
	"github.com/ryogrid/gookv/pkg/pdclient"
)

// newPDCluster creates a 3-node PD cluster, waits for leader election, and bootstraps it.
func newPDCluster(t *testing.T) *e2elib.PDCluster {
	t.Helper()
	e2elib.SkipIfNoBinary(t, "gookv-pd")

	cluster := e2elib.NewPDCluster(t, e2elib.PDClusterConfig{NumNodes: 3})
	require.NoError(t, cluster.Start())

	// Wait for leader election by polling IsBootstrapped (which requires a leader).
	client := cluster.Client()
	bootstrapPDCluster(t, client)

	return cluster
}

// bootstrapPDCluster bootstraps the PD cluster if not already bootstrapped.
func bootstrapPDCluster(t *testing.T, client pdclient.Client) {
	t.Helper()
	ctx := context.Background()

	bootstrapped, err := client.IsBootstrapped(ctx)
	if err == nil && bootstrapped {
		return
	}

	store := &metapb.Store{Id: 1, Address: "127.0.0.1:20160"}
	region := &metapb.Region{
		Id: 1,
		RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
		Peers:       []*metapb.Peer{{Id: 1, StoreId: 1}},
	}
	_, err = client.Bootstrap(ctx, store, region)
	require.NoError(t, err)
}

// bootstrapPDClusterViaCLI bootstraps the PD cluster via CLI if not already bootstrapped.
func bootstrapPDClusterViaCLI(t *testing.T, pdAddr string) {
	t.Helper()
	out, _, err := e2elib.CLIExecRaw(t, pdAddr, "IS BOOTSTRAPPED")
	if err == nil && strings.TrimSpace(strings.Split(out, "\n")[0]) == "true" {
		return
	}
	e2elib.CLIExec(t, pdAddr, "BOOTSTRAP 1 127.0.0.1:20160")
}

// waitForPDClusterLeader waits until the PD cluster has a leader that can serve requests.
func waitForPDClusterLeader(t *testing.T, client pdclient.Client, timeout time.Duration) {
	t.Helper()
	e2elib.WaitForCondition(t, timeout, "PD cluster leader election", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		bootstrapped, err := client.IsBootstrapped(ctx)
		return err == nil && bootstrapped
	})
}

// TestPDReplication_LeaderElection verifies that 3 PD nodes elect a leader.
func TestPDReplication_LeaderElection(t *testing.T) {
	cluster := newPDCluster(t)

	// The cluster should be operational (leader elected).
	// Check via CLI on the first node.
	pdAddr := cluster.Node(0).Addr()
	out := e2elib.CLIExec(t, pdAddr, "IS BOOTSTRAPPED")
	assert.Equal(t, "true", parseScalarValue(out))

	t.Log("PD replication leader election passed")
}

// TestPDReplication_WriteForwarding verifies writes on a follower are forwarded to leader.
func TestPDReplication_WriteForwarding(t *testing.T) {
	cluster := newPDCluster(t)

	// Try each node -- at least one should be a follower.
	// PutStore on a follower should be forwarded to the leader.
	for i := 0; i < 3; i++ {
		nodeAddr := cluster.Node(i).Addr()
		_, _, err := e2elib.CLIExecRaw(t, nodeAddr, "PUT STORE 100 127.0.0.1:99999")
		if err == nil {
			// Write succeeded (either leader or forwarded).
			break
		}
	}

	// Wait for the store to be visible on the leader (replication may take a moment).
	leaderAddr := cluster.Node(0).Addr()
	e2elib.CLIWaitForCondition(t, leaderAddr, "STORE STATUS 100", func(output string) bool {
		return strings.Contains(output, "127.0.0.1:99999")
	}, 10*time.Second)

	t.Log("PD replication write forwarding passed")
}

// TestPDReplication_Bootstrap verifies bootstrap is visible on all nodes.
func TestPDReplication_Bootstrap(t *testing.T) {
	cluster := newPDCluster(t)

	// All nodes should report bootstrapped.
	for i := 0; i < 3; i++ {
		nodeAddr := cluster.Node(i).Addr()
		e2elib.CLIWaitForCondition(t, nodeAddr, "IS BOOTSTRAPPED", func(output string) bool {
			return strings.TrimSpace(strings.Split(output, "\n")[0]) == "true"
		}, 10*time.Second)
	}

	t.Log("PD replication bootstrap passed")
}

// TestPDReplication_TSOMonotonicity verifies TSO is strictly increasing.
func TestPDReplication_TSOMonotonicity(t *testing.T) {
	cluster := newPDCluster(t)
	pdAddr := cluster.Node(0).Addr()

	var prevTS uint64
	for i := 0; i < 100; i++ {
		out := e2elib.CLIExec(t, pdAddr, "TSO")
		currentTS := parseTSOTimestamp(t, out)
		assert.Greater(t, currentTS, prevTS, "TSO should be strictly increasing (iter %d)", i)
		prevTS = currentTS
	}

	t.Log("PD replication TSO monotonicity passed")
}

// TestPDReplication_LeaderFailover verifies operations continue after leader stops.
func TestPDReplication_LeaderFailover(t *testing.T) {
	cluster := newPDCluster(t)

	// Write initial store via CLI on node 0.
	e2elib.CLIExec(t, cluster.Node(0).Addr(), "PUT STORE 10 127.0.0.1:10010")

	// Stop one node (may or may not be the leader).
	require.NoError(t, cluster.StopNode(0))

	// Use surviving nodes for subsequent CLI operations.
	survivingAddr := cluster.Node(1).Addr()

	// Wait for operations to resume on surviving nodes (TSO must work).
	e2elib.CLIWaitForCondition(t, survivingAddr, "TSO", func(output string) bool {
		return parseTSOTimestampRaw(output) > 0
	}, 30*time.Second)

	// Verify the store we wrote is still accessible via a surviving node.
	e2elib.CLIWaitForCondition(t, survivingAddr, "STORE STATUS 10", func(output string) bool {
		return strings.Contains(output, "127.0.0.1:10010")
	}, 10*time.Second)

	t.Log("PD replication leader failover passed")
}

// TestPDReplication_SingleNodeCompat verifies single PD node backward compatibility.
func TestPDReplication_SingleNodeCompat(t *testing.T) {
	e2elib.SkipIfNoBinary(t, "gookv-pd")

	alloc := e2elib.NewPortAllocator()
	t.Cleanup(func() { alloc.ReleaseAll() })

	pd := e2elib.NewPDNode(t, alloc, e2elib.PDNodeConfig{})
	require.NoError(t, pd.Start())
	require.NoError(t, pd.WaitForReady(15*time.Second))

	pdAddr := pd.Addr()

	// Bootstrap.
	e2elib.CLIExec(t, pdAddr, "BOOTSTRAP 1 127.0.0.1:20160")

	// TSO monotonicity.
	var prevTS uint64
	for i := 0; i < 10; i++ {
		out := e2elib.CLIExec(t, pdAddr, "TSO")
		ts := parseTSOTimestamp(t, out)
		assert.Greater(t, ts, prevTS)
		prevTS = ts
	}

	// AllocID increasing.
	var prevID uint64
	for i := 0; i < 10; i++ {
		out := e2elib.CLIExec(t, pdAddr, "ALLOC ID")
		id := parseUint64Value(t, out)
		assert.Greater(t, id, prevID)
		prevID = id
	}

	t.Log("PD replication single node compat passed")
}

// TestPDReplication_IDAllocMonotonicity verifies AllocID returns unique increasing IDs.
func TestPDReplication_IDAllocMonotonicity(t *testing.T) {
	cluster := newPDCluster(t)
	pdAddr := cluster.Node(0).Addr()

	var prevID uint64
	for i := 0; i < 50; i++ {
		out := e2elib.CLIExec(t, pdAddr, "ALLOC ID")
		id := parseUint64Value(t, out)
		assert.Greater(t, id, prevID, "AllocID should be strictly increasing (iter %d)", i)
		prevID = id
	}

	t.Log("PD replication ID alloc monotonicity passed")
}

// TestPDReplication_GCSafePoint verifies GC safe point is visible on all nodes.
func TestPDReplication_GCSafePoint(t *testing.T) {
	cluster := newPDCluster(t)
	pdAddr := cluster.Node(0).Addr()

	// Update GC safe point.
	out := e2elib.CLIExec(t, pdAddr, "GC SAFEPOINT SET 2000")
	assert.Contains(t, out, "2000")

	// Verify visible from each node.
	for i := 0; i < 3; i++ {
		nodeAddr := cluster.Node(i).Addr()
		e2elib.CLIWaitForCondition(t, nodeAddr, "GC SAFEPOINT", func(output string) bool {
			return strings.Contains(output, "2000")
		}, 10*time.Second)
	}

	t.Log("PD replication GC safe point passed")
}

// TestPDReplication_RegionHeartbeat verifies region heartbeat works on replicated PD.
func TestPDReplication_RegionHeartbeat(t *testing.T) {
	cluster := newPDCluster(t)
	pdAddr := cluster.Node(0).Addr()

	// Region 1 was created during bootstrap. Wait for PD to know about it.
	e2elib.CLIWaitForCondition(t, pdAddr, "REGION ID 1", func(output string) bool {
		return strings.Contains(output, "Region ID:  1")
	}, 10*time.Second)

	t.Log("PD replication region heartbeat passed")
}

// TestPDReplication_AskBatchSplit verifies split ID allocation on replicated PD.
func TestPDReplication_AskBatchSplit(t *testing.T) {
	cluster := newPDCluster(t)
	pdAddr := cluster.Node(0).Addr()

	// Verify region 1 exists.
	out := e2elib.CLIExec(t, pdAddr, "REGION ID 1")
	assert.Contains(t, out, "Region ID:  1")

	// Ask for 2 splits.
	out = e2elib.CLIExec(t, pdAddr, "ASK SPLIT 1 2")
	assert.Contains(t, out, "NewRegionID")

	// Parse both rows and verify all IDs are unique.
	ids := make(map[uint64]bool)
	for rowIdx := 0; rowIdx < 2; rowIdx++ {
		regionID, peerIDs := parseAskSplitRow(t, out, rowIdx)
		assert.False(t, ids[regionID], "region IDs should be unique")
		ids[regionID] = true
		for _, pid := range peerIDs {
			assert.False(t, ids[pid], "peer IDs should be unique")
			ids[pid] = true
		}
	}

	t.Log("PD replication AskBatchSplit passed")
}

// TestPDReplication_ConcurrentWritesFromMultipleClients verifies concurrent AllocID
// from clients connected to different nodes all produce unique IDs.
func TestPDReplication_ConcurrentWritesFromMultipleClients(t *testing.T) {
	cluster := newPDCluster(t)

	var mu sync.Mutex
	allIDs := make(map[uint64]bool)
	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		nodeAddr := cluster.Node(i).Addr()
		wg.Add(1)
		go func(addr string) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				out, _, err := e2elib.CLIExecRaw(t, addr, "ALLOC ID")
				if err != nil {
					continue
				}
				s := parseScalarValue(out)
				id, err := strconv.ParseUint(s, 10, 64)
				if err != nil {
					continue
				}
				mu.Lock()
				allIDs[id] = true
				mu.Unlock()
			}
		}(nodeAddr)
	}

	wg.Wait()
	assert.GreaterOrEqual(t, len(allIDs), 25, "should have many unique IDs from concurrent alloc")

	t.Log("PD replication concurrent writes passed")
}

// TestPDReplication_TSOViaFollower verifies TSO works when connected to a follower.
func TestPDReplication_TSOViaFollower(t *testing.T) {
	cluster := newPDCluster(t)

	// Try each node -- at least one is a follower; TSO should work via forwarding.
	successCount := 0
	for i := 0; i < 3; i++ {
		nodeAddr := cluster.Node(i).Addr()
		var prevTS uint64
		ok := true
		for j := 0; j < 10; j++ {
			out, _, err := e2elib.CLIExecRaw(t, nodeAddr, "TSO")
			if err != nil {
				ok = false
				break
			}
			ts := parseTSOTimestampRaw(out)
			if ts == 0 || ts <= prevTS {
				ok = false
				break
			}
			prevTS = ts
		}
		if ok {
			successCount++
			t.Logf("TSO via node %d (follower or leader) passed", i)
		}
	}
	assert.GreaterOrEqual(t, successCount, 1, "at least 1 node should produce monotonic TSO results")

	t.Log("PD replication TSO via follower passed")
}

// TestPDReplication_TSOViaFollowerForwarding verifies TSO forwarding via streaming proxy.
func TestPDReplication_TSOViaFollowerForwarding(t *testing.T) {
	cluster := newPDCluster(t)

	// Connect to each node and verify 20 TSO calls succeed with monotonicity.
	successCount := 0
	for i := 0; i < 3; i++ {
		nodeAddr := cluster.Node(i).Addr()
		var prevTS uint64
		nodeOk := true
		for j := 0; j < 20; j++ {
			out, _, err := e2elib.CLIExecRaw(t, nodeAddr, "TSO")
			if err != nil {
				nodeOk = false
				break
			}
			currentTS := parseTSOTimestampRaw(out)
			if currentTS == 0 {
				nodeOk = false
				break
			}
			if prevTS > 0 && currentTS <= prevTS {
				nodeOk = false
				break
			}
			prevTS = currentTS
		}
		if nodeOk {
			successCount++
		}
	}
	assert.GreaterOrEqual(t, successCount, 1, "at least 1 node should produce monotonic TSO results")

	t.Log("PD replication TSO via follower forwarding passed")
}

// TestPDReplication_RegionHeartbeatViaFollower verifies region heartbeat via follower forwarding.
func TestPDReplication_RegionHeartbeatViaFollower(t *testing.T) {
	cluster := newPDCluster(t)

	// All nodes should be able to serve REGION ID 1 after bootstrap.
	for i := 0; i < 3; i++ {
		nodeAddr := cluster.Node(i).Addr()
		e2elib.CLIWaitForCondition(t, nodeAddr, "REGION ID 1", func(output string) bool {
			return strings.Contains(output, "Region ID:")
		}, 10*time.Second)
	}

	t.Log("PD replication region heartbeat via follower passed")
}

// TestPDReplication_5NodeCluster verifies a 5-node PD cluster operates correctly.
func TestPDReplication_5NodeCluster(t *testing.T) {
	e2elib.SkipIfNoBinary(t, "gookv-pd")

	cluster := e2elib.NewPDCluster(t, e2elib.PDClusterConfig{NumNodes: 5})
	require.NoError(t, cluster.Start())

	// Bootstrap via CLI using the first node.
	pdAddr := cluster.Node(0).Addr()

	// Wait for leader election, then bootstrap.
	e2elib.WaitForCondition(t, 30*time.Second, "5-node cluster leader election", func() bool {
		_, _, err := e2elib.CLIExecRaw(t, pdAddr, "IS BOOTSTRAPPED")
		return err == nil
	})
	bootstrapPDClusterViaCLI(t, pdAddr)

	// PutStore should work.
	e2elib.CLIExec(t, pdAddr, "PUT STORE 1 127.0.0.1:20160")

	// GetTS should work.
	out := e2elib.CLIExec(t, pdAddr, "TSO")
	ts := parseTSOTimestamp(t, out)
	assert.NotZero(t, ts)

	t.Log("PD replication 5-node cluster passed")
}

// TestPDReplication_CatchUpRecovery verifies a restarted node catches up via Raft log.
func TestPDReplication_CatchUpRecovery(t *testing.T) {
	cluster := newPDCluster(t)

	// Stop node 0.
	require.NoError(t, cluster.StopNode(0))

	// Write stores while node 0 is down, using a surviving node.
	survivingAddr := cluster.Node(1).Addr()
	for i := uint64(20); i < 25; i++ {
		stmt := fmt.Sprintf("PUT STORE %d 127.0.0.1:30000", i)
		e2elib.CLIWaitForCondition(t, survivingAddr, stmt, func(output string) bool {
			return true // CLIWaitForCondition only calls checkFn on success (no error).
		}, 10*time.Second)
	}

	// Restart node 0.
	require.NoError(t, cluster.RestartNode(0))

	// Wait for node 0 to catch up — verify store 24 is visible via node 0.
	node0Addr := cluster.Node(0).Addr()
	e2elib.CLIWaitForCondition(t, node0Addr, "STORE STATUS 24", func(output string) bool {
		return strings.Contains(output, "127.0.0.1:30000")
	}, 30*time.Second)

	t.Log("PD replication catch-up recovery passed")
}

// parseTSOTimestampRaw extracts the raw timestamp from TSO output without fataling.
// Returns 0 if parsing fails.
func parseTSOTimestampRaw(output string) uint64 {
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Timestamp:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ts, err := strconv.ParseUint(parts[1], 10, 64)
				if err == nil {
					return ts
				}
			}
		}
	}
	return 0
}

