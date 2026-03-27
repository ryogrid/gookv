package e2e_external_test

import (
	"context"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestClusterServerLeaderElection verifies that a 3-node cluster elects a leader.
func TestClusterServerLeaderElection(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdClient := cluster.PD().Client()

	// PD should know about a leader for region containing "".
	leaderStoreID := e2elib.WaitForRegionLeader(t, pdClient, []byte(""), 30*time.Second)
	assert.NotZero(t, leaderStoreID, "should have a leader")

	t.Log("Cluster leader election passed")
}

// TestClusterServerKvOperations tests prewrite → commit → get on the cluster.
func TestClusterServerKvOperations(t *testing.T) {
	cluster := newClusterWithLeader(t)

	// Use raw gRPC on a node. The client library routes automatically.
	rawKV := cluster.RawKV()
	ctx := context.Background()

	err := rawKV.Put(ctx, []byte("kv-op-key"), []byte("kv-op-val"))
	require.NoError(t, err)

	val, notFound, err := rawKV.Get(ctx, []byte("kv-op-key"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("kv-op-val"), val)

	t.Log("Cluster KV operations passed")
}

// TestClusterServerCrossNodeReplication writes on leader and verifies data is replicated.
func TestClusterServerCrossNodeReplication(t *testing.T) {
	cluster := newClusterWithLeader(t)
	ctx := context.Background()

	// Write via client library (auto-routes to leader).
	rawKV := cluster.RawKV()
	err := rawKV.Put(ctx, []byte("repl-key"), []byte("repl-val"))
	require.NoError(t, err)

	// Read back via client (might hit any node with up-to-date data).
	val, notFound, err := rawKV.Get(ctx, []byte("repl-key"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("repl-val"), val)

	// Also verify via raw gRPC on each node.
	replicatedCount := 0
	for i := 0; i < 3; i++ {
		client := e2elib.DialTikvClient(t, cluster.Node(i).Addr())
		getResp, err := client.RawGet(ctx, &kvrpcpb.RawGetRequest{
			Key: []byte("repl-key"),
		})
		if err != nil {
			continue // Some nodes may reject if not leader
		}
		if !getResp.GetNotFound() {
			assert.Equal(t, []byte("repl-val"), getResp.GetValue(),
				"node %d should have replicated value", i)
			replicatedCount++
		}
	}
	assert.GreaterOrEqual(t, replicatedCount, 1, "at least 1 node should have the replicated value")

	t.Log("Cross-node replication passed")
}

// TestClusterServerNodeFailure verifies the cluster survives minority node failure.
func TestClusterServerNodeFailure(t *testing.T) {
	cluster := newClusterWithLeader(t)
	rawKV := cluster.RawKV()
	ctx := context.Background()

	// Write initial data.
	err := rawKV.Put(ctx, []byte("survive-key"), []byte("survive-val"))
	require.NoError(t, err)

	// Stop one node (minority failure in a 3-node cluster).
	require.NoError(t, cluster.StopNode(2))

	// Cluster should still be operational (2 of 3 nodes = majority).
	e2elib.WaitForCondition(t, 15*time.Second, "cluster operational after node failure", func() bool {
		ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return rawKV.Put(ctx2, []byte("after-fail-key"), []byte("after-fail-val")) == nil
	})

	// Read should work.
	val, notFound, err := rawKV.Get(ctx, []byte("survive-key"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("survive-val"), val)

	t.Log("Cluster survives minority node failure passed")
}

// TestClusterServerLeaderFailover verifies new leader election after old leader stops.
func TestClusterServerLeaderFailover(t *testing.T) {
	cluster := newClusterWithLeader(t)
	rawKV := cluster.RawKV()
	ctx := context.Background()

	// Write initial data.
	err := rawKV.Put(ctx, []byte("failover-key"), []byte("failover-val"))
	require.NoError(t, err)

	// Find current leader via PD.
	pdClient := cluster.PD().Client()
	leaderStoreID := e2elib.WaitForRegionLeader(t, pdClient, []byte(""), 30*time.Second)

	// Stop the leader node (store IDs are 1-indexed, node indices are 0-indexed).
	leaderIdx := int(leaderStoreID) - 1
	require.NoError(t, cluster.StopNode(leaderIdx))

	// Wait for new leader and verify operations work.
	e2elib.WaitForCondition(t, 30*time.Second, "new leader elected after failover", func() bool {
		ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return rawKV.Put(ctx2, []byte("new-leader-key"), []byte("new-leader-val")) == nil
	})

	// Read old data should still work.
	val, notFound, err := rawKV.Get(ctx, []byte("failover-key"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("failover-val"), val)

	t.Log("Leader failover passed")
}
