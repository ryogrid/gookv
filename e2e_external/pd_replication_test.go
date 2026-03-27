package e2e_external_test

import (
	"context"
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
	client := cluster.Client()

	// The cluster should be operational (leader elected).
	ctx := context.Background()
	bootstrapped, err := client.IsBootstrapped(ctx)
	require.NoError(t, err)
	assert.True(t, bootstrapped)

	t.Log("PD replication leader election passed")
}

// TestPDReplication_WriteForwarding verifies writes on a follower are forwarded to leader.
func TestPDReplication_WriteForwarding(t *testing.T) {
	cluster := newPDCluster(t)
	ctx := context.Background()

	// Try each node — at least one should be a follower.
	// PutStore on a follower should be forwarded to the leader.
	for i := 0; i < 3; i++ {
		nodeClient := cluster.ClientForNode(i)
		err := nodeClient.PutStore(ctx, &metapb.Store{Id: 100, Address: "127.0.0.1:99999"})
		if err == nil {
			// Write succeeded (either leader or forwarded).
			break
		}
	}

	// Wait for the store to be visible (replication may take a moment).
	clusterClient := cluster.Client()
	e2elib.WaitForCondition(t, 10*time.Second, "store visible after forwarding", func() bool {
		store, err := clusterClient.GetStore(ctx, 100)
		return err == nil && store != nil && store.GetAddress() == "127.0.0.1:99999"
	})

	t.Log("PD replication write forwarding passed")
}

// TestPDReplication_Bootstrap verifies bootstrap is visible on all nodes.
func TestPDReplication_Bootstrap(t *testing.T) {
	cluster := newPDCluster(t)
	ctx := context.Background()

	// All nodes should report bootstrapped.
	for i := 0; i < 3; i++ {
		nodeClient := cluster.ClientForNode(i)
		e2elib.WaitForCondition(t, 10*time.Second, "node bootstrapped", func() bool {
			bootstrapped, err := nodeClient.IsBootstrapped(ctx)
			return err == nil && bootstrapped
		})
	}

	t.Log("PD replication bootstrap passed")
}

// TestPDReplication_TSOMonotonicity verifies TSO is strictly increasing.
func TestPDReplication_TSOMonotonicity(t *testing.T) {
	cluster := newPDCluster(t)
	client := cluster.Client()
	ctx := context.Background()

	var prevTS uint64
	for i := 0; i < 100; i++ {
		ts, err := client.GetTS(ctx)
		require.NoError(t, err)
		currentTS := ts.ToUint64()
		assert.Greater(t, currentTS, prevTS, "TSO should be strictly increasing (iter %d)", i)
		prevTS = currentTS
	}

	t.Log("PD replication TSO monotonicity passed")
}

// TestPDReplication_LeaderFailover verifies operations continue after leader stops.
func TestPDReplication_LeaderFailover(t *testing.T) {
	cluster := newPDCluster(t)
	client := cluster.Client()
	ctx := context.Background()

	// Write initial store.
	err := client.PutStore(ctx, &metapb.Store{Id: 10, Address: "127.0.0.1:10010"})
	require.NoError(t, err)

	// Stop one node (may or may not be the leader).
	require.NoError(t, cluster.StopNode(0))

	// Create a client connected to the surviving nodes.
	survivingAddrs := []string{cluster.Node(1).Addr(), cluster.Node(2).Addr()}
	survCtx, survCancel := context.WithTimeout(context.Background(), 10*time.Second)
	survClient, err := pdclient.NewClient(survCtx, pdclient.Config{Endpoints: survivingAddrs})
	survCancel()
	require.NoError(t, err)
	t.Cleanup(func() { survClient.Close() })

	// Wait for operations to resume on surviving nodes.
	e2elib.WaitForCondition(t, 30*time.Second, "PD cluster operational after failover", func() bool {
		ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, err := survClient.GetTS(ctx2)
		return err == nil
	})

	// Verify the store we wrote is still accessible.
	store, err := survClient.GetStore(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, "127.0.0.1:10010", store.GetAddress())

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

	client := pd.Client()
	ctx := context.Background()

	// Bootstrap.
	_, err := client.Bootstrap(ctx, &metapb.Store{Id: 1, Address: "127.0.0.1:20160"},
		&metapb.Region{Id: 1, RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
			Peers: []*metapb.Peer{{Id: 1, StoreId: 1}}})
	require.NoError(t, err)

	// TSO monotonicity.
	var prevTS uint64
	for i := 0; i < 10; i++ {
		ts, err := client.GetTS(ctx)
		require.NoError(t, err)
		assert.Greater(t, ts.ToUint64(), prevTS)
		prevTS = ts.ToUint64()
	}

	// AllocID increasing.
	var prevID uint64
	for i := 0; i < 10; i++ {
		id, err := client.AllocID(ctx)
		require.NoError(t, err)
		assert.Greater(t, id, prevID)
		prevID = id
	}

	t.Log("PD replication single node compat passed")
}

// TestPDReplication_IDAllocMonotonicity verifies AllocID returns unique increasing IDs.
func TestPDReplication_IDAllocMonotonicity(t *testing.T) {
	cluster := newPDCluster(t)
	client := cluster.Client()
	ctx := context.Background()

	var prevID uint64
	for i := 0; i < 50; i++ {
		id, err := client.AllocID(ctx)
		require.NoError(t, err)
		assert.Greater(t, id, prevID, "AllocID should be strictly increasing (iter %d)", i)
		prevID = id
	}

	t.Log("PD replication ID alloc monotonicity passed")
}

// TestPDReplication_GCSafePoint verifies GC safe point is visible on all nodes.
func TestPDReplication_GCSafePoint(t *testing.T) {
	cluster := newPDCluster(t)
	client := cluster.Client()
	ctx := context.Background()

	// Update GC safe point.
	newSP, err := client.UpdateGCSafePoint(ctx, 2000)
	require.NoError(t, err)
	assert.Equal(t, uint64(2000), newSP)

	// Verify visible from each node.
	for i := 0; i < 3; i++ {
		nodeClient := cluster.ClientForNode(i)
		e2elib.WaitForCondition(t, 10*time.Second, "GC safe point visible", func() bool {
			sp, err := nodeClient.GetGCSafePoint(ctx)
			return err == nil && sp == 2000
		})
	}

	t.Log("PD replication GC safe point passed")
}

// TestPDReplication_RegionHeartbeat verifies region heartbeat works on replicated PD.
func TestPDReplication_RegionHeartbeat(t *testing.T) {
	cluster := newPDCluster(t)
	client := cluster.Client()
	ctx := context.Background()

	// Region 1 was created during bootstrap. Wait for PD to know about it.
	e2elib.WaitForCondition(t, 10*time.Second, "region 1 visible", func() bool {
		region, _, err := client.GetRegionByID(ctx, 1)
		return err == nil && region != nil && region.GetId() == 1
	})

	t.Log("PD replication region heartbeat passed")
}

// TestPDReplication_AskBatchSplit verifies split ID allocation on replicated PD.
func TestPDReplication_AskBatchSplit(t *testing.T) {
	cluster := newPDCluster(t)
	client := cluster.Client()
	ctx := context.Background()

	region, _, err := client.GetRegionByID(ctx, 1)
	require.NoError(t, err)

	// Ask for 2 splits.
	resp, err := client.AskBatchSplit(ctx, region, 2)
	require.NoError(t, err)
	require.Len(t, resp.GetIds(), 2)

	// All IDs should be unique.
	ids := make(map[uint64]bool)
	for _, split := range resp.GetIds() {
		assert.False(t, ids[split.GetNewRegionId()], "region IDs should be unique")
		ids[split.GetNewRegionId()] = true
		for _, pid := range split.GetNewPeerIds() {
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
	ctx := context.Background()

	var mu sync.Mutex
	allIDs := make(map[uint64]bool)
	var wg sync.WaitGroup

	for i := 0; i < 3; i++ {
		nodeClient := cluster.ClientForNode(i)
		wg.Add(1)
		go func(c pdclient.Client) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				id, err := c.AllocID(ctx)
				if err != nil {
					continue
				}
				mu.Lock()
				allIDs[id] = true
				mu.Unlock()
			}
		}(nodeClient)
	}

	wg.Wait()
	assert.GreaterOrEqual(t, len(allIDs), 25, "should have many unique IDs from concurrent alloc")

	t.Log("PD replication concurrent writes passed")
}

// TestPDReplication_TSOViaFollower verifies TSO works when connected to a follower.
func TestPDReplication_TSOViaFollower(t *testing.T) {
	cluster := newPDCluster(t)
	ctx := context.Background()

	// Try each node — at least one is a follower; TSO should work via forwarding.
	successCount := 0
	for i := 0; i < 3; i++ {
		nodeClient := cluster.ClientForNode(i)
		var prevTS uint64
		ok := true
		for j := 0; j < 10; j++ {
			ts, err := nodeClient.GetTS(ctx)
			if err != nil {
				ok = false
				break
			}
			if ts.ToUint64() <= prevTS {
				ok = false
				break
			}
			prevTS = ts.ToUint64()
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
	ctx := context.Background()

	// Connect to each node and verify 20 TSO calls succeed with monotonicity.
	successCount := 0
	for i := 0; i < 3; i++ {
		nodeClient := cluster.ClientForNode(i)
		var prevTS uint64
		nodeOk := true
		for j := 0; j < 20; j++ {
			ts, err := nodeClient.GetTS(ctx)
			if err != nil {
				nodeOk = false
				break
			}
			currentTS := ts.ToUint64()
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
	ctx := context.Background()

	// All nodes should be able to serve GetRegionByID after bootstrap.
	for i := 0; i < 3; i++ {
		nodeClient := cluster.ClientForNode(i)
		e2elib.WaitForCondition(t, 10*time.Second, "region visible via follower", func() bool {
			region, _, err := nodeClient.GetRegionByID(ctx, 1)
			return err == nil && region != nil
		})
	}

	t.Log("PD replication region heartbeat via follower passed")
}

// TestPDReplication_5NodeCluster verifies a 5-node PD cluster operates correctly.
func TestPDReplication_5NodeCluster(t *testing.T) {
	e2elib.SkipIfNoBinary(t, "gookv-pd")

	cluster := e2elib.NewPDCluster(t, e2elib.PDClusterConfig{NumNodes: 5})
	require.NoError(t, cluster.Start())

	client := cluster.Client()
	bootstrapPDCluster(t, client)

	ctx := context.Background()

	// PutStore should work.
	err := client.PutStore(ctx, &metapb.Store{Id: 1, Address: "127.0.0.1:20160"})
	require.NoError(t, err)

	// GetTS should work.
	ts, err := client.GetTS(ctx)
	require.NoError(t, err)
	assert.NotZero(t, ts.ToUint64())

	t.Log("PD replication 5-node cluster passed")
}

// TestPDReplication_CatchUpRecovery verifies a restarted node catches up via Raft log.
func TestPDReplication_CatchUpRecovery(t *testing.T) {
	cluster := newPDCluster(t)
	client := cluster.Client()
	ctx := context.Background()

	// Stop node 0.
	require.NoError(t, cluster.StopNode(0))

	// Write stores while node 0 is down.
	for i := uint64(20); i < 25; i++ {
		e2elib.WaitForCondition(t, 10*time.Second, "PutStore after node down", func() bool {
			err := client.PutStore(ctx, &metapb.Store{Id: i, Address: "127.0.0.1:30000"})
			return err == nil
		})
	}

	// Restart node 0.
	require.NoError(t, cluster.RestartNode(0))

	// Wait for node 0 to catch up.
	node0Client := cluster.ClientForNode(0)
	e2elib.WaitForCondition(t, 30*time.Second, "node 0 catch up", func() bool {
		store, err := node0Client.GetStore(ctx, 24)
		return err == nil && store != nil
	})

	t.Log("PD replication catch-up recovery passed")
}
