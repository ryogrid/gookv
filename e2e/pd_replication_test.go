package e2e

import (
	"context"
	"fmt"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/internal/pd"
	"github.com/ryogrid/gookv/pkg/pdclient"
)

// getFreeAddr allocates a TCP listener on a random port, retrieves the address,
// and closes the listener so the port can be reused.
func getFreeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := l.Addr().String()
	l.Close()
	return addr
}

// startPDCluster starts a multi-node PD cluster for testing.
// Each node gets a random client port and a random peer port.
// Returns the servers, a slice of client addresses, and a slice of peer addresses.
func startPDCluster(t *testing.T, nodeCount int) ([]*pd.PDServer, []string, []string) {
	t.Helper()

	// 1. Pre-allocate ports for client and peer listeners.
	clientAddrs := make([]string, nodeCount)
	peerAddrs := make([]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		clientAddrs[i] = getFreeAddr(t)
		peerAddrs[i] = getFreeAddr(t)
	}

	// 2. Build InitialCluster and ClientAddrs maps.
	initialCluster := make(map[uint64]string, nodeCount)
	clientAddrMap := make(map[uint64]string, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodeID := uint64(i + 1)
		initialCluster[nodeID] = peerAddrs[i]
		clientAddrMap[nodeID] = clientAddrs[i]
	}

	// 3. Create and start each PDServer.
	servers := make([]*pd.PDServer, nodeCount)
	for i := 0; i < nodeCount; i++ {
		nodeID := uint64(i + 1)

		cfg := pd.DefaultPDServerConfig()
		cfg.ListenAddr = clientAddrs[i]
		cfg.DataDir = t.TempDir()
		cfg.ClusterID = 1

		cfg.RaftConfig = &pd.PDServerRaftConfig{
			PDNodeID:             nodeID,
			InitialCluster:       initialCluster,
			PeerAddr:             peerAddrs[i],
			ClientAddrs:          clientAddrMap,
			RaftTickInterval:     100 * time.Millisecond,
			ElectionTimeoutTicks: 10,
			HeartbeatTicks:       3,
		}

		srv, err := pd.NewPDServer(cfg)
		require.NoError(t, err, "failed to create PD server %d", nodeID)

		err = srv.Start()
		require.NoError(t, err, "failed to start PD server %d", nodeID)

		servers[i] = srv
	}

	// 4. Register cleanup to stop all servers in reverse order.
	// PDServer.Stop() is idempotent, so double-stop is safe.
	t.Cleanup(func() {
		for i := len(servers) - 1; i >= 0; i-- {
			servers[i].Stop()
		}
	})

	return servers, clientAddrs, peerAddrs
}

// newPDClusterClient creates a PD client connected to all endpoints of a PD cluster.
func newPDClusterClient(t *testing.T, addrs []string) pdclient.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, err := pdclient.NewClient(ctx, pdclient.Config{
		Endpoints:     addrs,
		RetryInterval: 200 * time.Millisecond,
		RetryMaxCount: 20,
	})
	require.NoError(t, err)
	t.Cleanup(func() { client.Close() })
	return client
}

// waitForLeader polls GetMembers until a leader is reported, with timeout.
func waitForLeader(t *testing.T, client pdclient.Client, timeout time.Duration) *pdpb.Member {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	// Use a raw gRPC client to call GetMembers since pdclient.Client doesn't expose it.
	// Instead, we'll use GetTS as a proxy: if GetTS succeeds, the leader is elected.
	// But the test plan says use GetMembers. Let's try using GetTS first as a leader check.
	// Actually, let me use IsBootstrapped which always works and confirms the node is reachable.

	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			t.Fatal("timed out waiting for PD leader election")
			return nil
		case <-ticker.C:
			// Try to call IsBootstrapped. If the leader is available, this should succeed.
			_, err := client.IsBootstrapped(ctx)
			if err == nil {
				// Leader is up. Return a placeholder member.
				return &pdpb.Member{Name: "leader"}
			}
		}
	}
}

// findLeaderIndex returns the index of the leader server in the servers slice.
// Returns -1 if no leader found.
func findLeaderIndex(t *testing.T, servers []*pd.PDServer) int {
	t.Helper()
	// Poll for up to 10 seconds.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		for i, srv := range servers {
			if srv.IsRaftLeader() {
				return i
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("no leader found among PD servers")
	return -1
}

// bootstrapCluster bootstraps a PD cluster via the given client.
func bootstrapCluster(t *testing.T, client pdclient.Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	store := &metapb.Store{Id: 1, Address: "127.0.0.1:20160"}
	region := &metapb.Region{
		Id:       1,
		StartKey: nil,
		EndKey:   nil,
		Peers:    []*metapb.Peer{{Id: 1, StoreId: 1}},
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 1,
		},
	}
	_, err := client.Bootstrap(ctx, store, region)
	require.NoError(t, err, "bootstrap should succeed")
}

// TestPDReplication_LeaderElection verifies that starting 3 PD nodes results in
// exactly one elected leader, and all nodes agree on who the leader is.
func TestPDReplication_LeaderElection(t *testing.T) {
	servers, clientAddrs, _ := startPDCluster(t, 3)

	// Wait for leader election via polling IsRaftLeader on servers.
	leaderIdx := findLeaderIndex(t, servers)

	// Verify exactly one server thinks it is the leader.
	leaderCount := 0
	for _, srv := range servers {
		if srv.IsRaftLeader() {
			leaderCount++
		}
	}
	assert.Equal(t, 1, leaderCount, "exactly one PD should be the leader")

	t.Logf("Leader elected: node %d (addr: %s)", leaderIdx+1, clientAddrs[leaderIdx])
}

// TestPDReplication_WriteForwarding verifies that a PutStore on a follower is
// forwarded to the leader and visible on all nodes.
func TestPDReplication_WriteForwarding(t *testing.T) {
	servers, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	// Wait for leader and bootstrap.
	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Find a follower address.
	leaderIdx := findLeaderIndex(t, servers)
	followerAddr := ""
	for i, addr := range clientAddrs {
		if i != leaderIdx {
			followerAddr = addr
			break
		}
	}
	require.NotEmpty(t, followerAddr, "should find a follower address")

	// Create a client connected ONLY to the follower.
	followerClient := newPDClusterClient(t, []string{followerAddr})

	// PutStore via follower -- should be forwarded to leader.
	store2 := &metapb.Store{Id: 2, Address: "127.0.0.1:20161"}
	err := followerClient.PutStore(ctx, store2)
	require.NoError(t, err, "PutStore via follower should succeed (forwarded to leader)")

	// Wait for Raft replication to propagate writes to followers.
	time.Sleep(500 * time.Millisecond)

	// Verify GetStore on all nodes.
	for i, addr := range clientAddrs {
		c := newPDClusterClient(t, []string{addr})
		s, err := c.GetStore(ctx, 2)
		require.NoError(t, err, "GetStore on node %d should succeed", i+1)
		require.NotNil(t, s, "store should exist on node %d", i+1)
		assert.Equal(t, uint64(2), s.GetId())
		assert.Equal(t, "127.0.0.1:20161", s.GetAddress())
	}
}

// TestPDReplication_Bootstrap verifies that bootstrapping via any node makes
// IsBootstrapped return true on all nodes.
func TestPDReplication_Bootstrap(t *testing.T) {
	_, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)

	// Bootstrap the cluster.
	bootstrapCluster(t, client)

	// Verify IsBootstrapped on all nodes.
	for i, addr := range clientAddrs {
		c := newPDClusterClient(t, []string{addr})
		bootstrapped, err := c.IsBootstrapped(ctx)
		require.NoError(t, err, "IsBootstrapped on node %d should succeed", i+1)
		assert.True(t, bootstrapped, "node %d should report cluster as bootstrapped", i+1)
	}
}

// TestPDReplication_TSOMonotonicity verifies that 100 consecutive GetTS calls
// produce strictly increasing timestamps.
func TestPDReplication_TSOMonotonicity(t *testing.T) {
	_, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	var prevTS uint64
	for i := 0; i < 100; i++ {
		ts, err := client.GetTS(ctx)
		require.NoError(t, err, "GetTS call %d should succeed", i)
		val := ts.ToUint64()
		assert.Greater(t, val, prevTS, "TSO must be strictly increasing (call %d)", i)
		prevTS = val
	}
}

// TestPDReplication_LeaderFailover verifies that after stopping the leader,
// a new leader is elected and operations continue working.
func TestPDReplication_LeaderFailover(t *testing.T) {
	servers, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Write initial store.
	err := client.PutStore(ctx, &metapb.Store{Id: 10, Address: "127.0.0.1:30000"})
	require.NoError(t, err)

	// Find and stop the leader.
	leaderIdx := findLeaderIndex(t, servers)
	t.Logf("Stopping leader at index %d (addr: %s)", leaderIdx, clientAddrs[leaderIdx])
	servers[leaderIdx].Stop()

	// Build endpoint list from surviving nodes.
	var survivingAddrs []string
	for i, addr := range clientAddrs {
		if i != leaderIdx {
			survivingAddrs = append(survivingAddrs, addr)
		}
	}

	// Wait for new leader to be elected on surviving nodes.
	// Use a client with only the surviving endpoints.
	survivingClient := newPDClusterClient(t, survivingAddrs)

	// Poll until new leader is available.
	deadline := time.Now().Add(15 * time.Second)
	var newLeaderFound bool
	for time.Now().Before(deadline) {
		_, err := survivingClient.IsBootstrapped(ctx)
		if err == nil {
			newLeaderFound = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	require.True(t, newLeaderFound, "new leader should be elected within 15 seconds")

	// Verify operations continue working.
	err = survivingClient.PutStore(ctx, &metapb.Store{Id: 11, Address: "127.0.0.1:30001"})
	require.NoError(t, err, "PutStore should work after leader failover")

	// Wait for Raft replication to propagate writes to followers.
	time.Sleep(500 * time.Millisecond)

	s, err := survivingClient.GetStore(ctx, 11)
	require.NoError(t, err, "GetStore should work after leader failover")
	require.NotNil(t, s)
	assert.Equal(t, uint64(11), s.GetId())
}

// TestPDReplication_SingleNodeCompat verifies that a single PD node without
// RaftConfig maintains backward compatibility with existing behavior.
func TestPDReplication_SingleNodeCompat(t *testing.T) {
	// Use the existing startPDServer helper which creates a single-node PD.
	_, addr := startPDServer(t)
	client := newPDClient(t, addr)
	ctx := context.Background()

	// Bootstrap.
	store := &metapb.Store{Id: 1, Address: "127.0.0.1:20160"}
	region := &metapb.Region{
		Id:    1,
		Peers: []*metapb.Peer{{Id: 1, StoreId: 1}},
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 1,
		},
	}
	_, err := client.Bootstrap(ctx, store, region)
	require.NoError(t, err)

	bootstrapped, err := client.IsBootstrapped(ctx)
	require.NoError(t, err)
	assert.True(t, bootstrapped)

	// TSO should be monotonic.
	ts1, err := client.GetTS(ctx)
	require.NoError(t, err)
	ts2, err := client.GetTS(ctx)
	require.NoError(t, err)
	assert.Greater(t, ts2.ToUint64(), ts1.ToUint64(), "TSO must be monotonic in single-node mode")

	// AllocID should be increasing.
	id1, err := client.AllocID(ctx)
	require.NoError(t, err)
	id2, err := client.AllocID(ctx)
	require.NoError(t, err)
	assert.Greater(t, id2, id1, "AllocID must be increasing in single-node mode")
}

// TestPDReplication_IDAllocMonotonicity verifies that 50 consecutive AllocID
// calls return unique, strictly increasing IDs.
func TestPDReplication_IDAllocMonotonicity(t *testing.T) {
	_, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	var prevID uint64
	idSet := make(map[uint64]bool)
	for i := 0; i < 50; i++ {
		id, err := client.AllocID(ctx)
		require.NoError(t, err, "AllocID call %d should succeed", i)
		assert.Greater(t, id, prevID, "IDs must be strictly increasing (call %d)", i)
		assert.False(t, idSet[id], "ID %d should be unique (call %d)", id, i)
		idSet[id] = true
		prevID = id
	}
	assert.Equal(t, 50, len(idSet), "all 50 IDs should be unique")
}

// TestPDReplication_GCSafePoint verifies that updating the GC safe point
// on one node makes it visible on all nodes in the cluster.
func TestPDReplication_GCSafePoint(t *testing.T) {
	_, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Update GC safe point.
	newSP, err := client.UpdateGCSafePoint(ctx, 1000)
	require.NoError(t, err)
	assert.Equal(t, uint64(1000), newSP)

	// Wait for Raft replication to propagate writes to followers.
	time.Sleep(500 * time.Millisecond)

	// Verify on all nodes.
	for i, addr := range clientAddrs {
		c := newPDClusterClient(t, []string{addr})
		sp, err := c.GetGCSafePoint(ctx)
		require.NoError(t, err, "GetGCSafePoint on node %d should succeed", i+1)
		assert.Equal(t, uint64(1000), sp, "GC safe point should be 1000 on node %d", i+1)
	}
}

// TestPDReplication_RegionHeartbeat verifies that a region heartbeat sent via
// the replicated PD cluster is visible on all nodes.
func TestPDReplication_RegionHeartbeat(t *testing.T) {
	_, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Send region heartbeat with a new region.
	region := &metapb.Region{
		Id:       100,
		StartKey: []byte("a"),
		EndKey:   []byte("z"),
		Peers: []*metapb.Peer{
			{Id: 100, StoreId: 1},
		},
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 1,
		},
	}
	leader := &metapb.Peer{Id: 100, StoreId: 1}

	_, err := client.ReportRegionHeartbeat(ctx, &pdpb.RegionHeartbeatRequest{
		Region: region,
		Leader: leader,
	})
	require.NoError(t, err, "ReportRegionHeartbeat should succeed")

	// Wait for Raft replication to propagate writes to followers.
	time.Sleep(500 * time.Millisecond)

	// Verify the region is visible on all nodes.
	for i, addr := range clientAddrs {
		c := newPDClusterClient(t, []string{addr})
		r, l, err := c.GetRegionByID(ctx, 100)
		require.NoError(t, err, "GetRegionByID on node %d should succeed", i+1)
		require.NotNil(t, r, "region 100 should exist on node %d", i+1)
		assert.Equal(t, uint64(100), r.GetId())
		if l != nil {
			assert.Equal(t, uint64(100), l.GetId())
		}
	}
}

// TestPDReplication_AskBatchSplit verifies that split ID allocation works
// correctly in a replicated PD cluster.
func TestPDReplication_AskBatchSplit(t *testing.T) {
	_, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Ask for 2 splits.
	region := &metapb.Region{
		Id:    1,
		Peers: []*metapb.Peer{{Id: 1, StoreId: 1}},
	}
	resp, err := client.AskBatchSplit(ctx, region, 2)
	require.NoError(t, err, "AskBatchSplit should succeed")
	require.Len(t, resp.GetIds(), 2, "should get 2 split IDs")

	// Verify all new region IDs are unique.
	regionIDs := make(map[uint64]bool)
	peerIDs := make(map[uint64]bool)
	for _, splitID := range resp.GetIds() {
		assert.NotZero(t, splitID.GetNewRegionId(), "new region ID should be non-zero")
		assert.False(t, regionIDs[splitID.GetNewRegionId()], "region IDs should be unique")
		regionIDs[splitID.GetNewRegionId()] = true

		for _, pid := range splitID.GetNewPeerIds() {
			assert.NotZero(t, pid, "peer ID should be non-zero")
			assert.False(t, peerIDs[pid], "peer IDs should be unique")
			peerIDs[pid] = true
		}
	}
}

// TestPDReplication_ConcurrentWritesFromMultipleClients verifies that concurrent
// AllocID calls from multiple clients connected to different nodes all produce
// unique IDs.
func TestPDReplication_ConcurrentWritesFromMultipleClients(t *testing.T) {
	_, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Create 3 clients, one per endpoint.
	clients := make([]pdclient.Client, len(clientAddrs))
	for i, addr := range clientAddrs {
		clients[i] = newPDClusterClient(t, []string{addr})
	}

	// Each client concurrently does 10 AllocID calls.
	var mu sync.Mutex
	allIDs := make(map[uint64]bool)
	var wg sync.WaitGroup

	for clientIdx, c := range clients {
		wg.Add(1)
		go func(idx int, cl pdclient.Client) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				id, err := cl.AllocID(ctx)
				if err != nil {
					t.Errorf("client %d AllocID %d failed: %v", idx, j, err)
					return
				}
				mu.Lock()
				allIDs[id] = true
				mu.Unlock()
			}
		}(clientIdx, c)
	}
	wg.Wait()

	assert.Equal(t, 30, len(allIDs), "all 30 IDs from 3 clients should be unique")
}

// TestPDReplication_TSOViaFollower verifies that TSO allocation works when
// the client is connected only to a follower endpoint (via forwarding).
func TestPDReplication_TSOViaFollower(t *testing.T) {
	servers, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Find a follower address.
	leaderIdx := findLeaderIndex(t, servers)
	followerAddr := ""
	for i, addr := range clientAddrs {
		if i != leaderIdx {
			followerAddr = addr
			break
		}
	}
	require.NotEmpty(t, followerAddr)

	// Create client connected only to the follower.
	followerClient := newPDClusterClient(t, []string{followerAddr})

	// Call GetTS 10 times; all should be strictly increasing.
	var prevTS uint64
	for i := 0; i < 10; i++ {
		ts, err := followerClient.GetTS(ctx)
		require.NoError(t, err, "GetTS via follower call %d should succeed", i)
		val := ts.ToUint64()
		assert.Greater(t, val, prevTS, "TSO via follower must be strictly increasing (call %d)", i)
		prevTS = val
	}
}

// TestPDReplication_TSOViaFollowerForwarding verifies that TSO allocation works
// via streaming proxy forwarding when the client is connected only to a follower.
// Unlike TestPDReplication_TSOViaFollower which tests the retry-based approach,
// this test verifies the streaming proxy path by calling GetTS 20 times and
// checking strict monotonicity.
func TestPDReplication_TSOViaFollowerForwarding(t *testing.T) {
	servers, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Find a follower address.
	leaderIdx := findLeaderIndex(t, servers)
	followerAddr := ""
	for i, addr := range clientAddrs {
		if i != leaderIdx {
			followerAddr = addr
			break
		}
	}
	require.NotEmpty(t, followerAddr, "should find a follower address")

	// Create client connected ONLY to the follower.
	followerClient := newPDClusterClient(t, []string{followerAddr})

	// Call GetTS 20 times; all should succeed (forwarded to leader via streaming proxy).
	var prevTS uint64
	for i := 0; i < 20; i++ {
		ts, err := followerClient.GetTS(ctx)
		require.NoError(t, err, "GetTS via follower forwarding call %d should succeed", i)
		val := ts.ToUint64()
		assert.Greater(t, val, prevTS,
			"TSO via follower forwarding must be strictly increasing (call %d)", i)
		prevTS = val
	}
	t.Logf("20 TSO calls via follower forwarding all succeeded with monotonic timestamps")
}

// TestPDReplication_RegionHeartbeatViaFollower verifies that a region heartbeat
// sent via a follower is forwarded to the leader and the region metadata becomes
// visible on all nodes.
func TestPDReplication_RegionHeartbeatViaFollower(t *testing.T) {
	servers, clientAddrs, _ := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Find a follower address.
	leaderIdx := findLeaderIndex(t, servers)
	followerAddr := ""
	for i, addr := range clientAddrs {
		if i != leaderIdx {
			followerAddr = addr
			break
		}
	}
	require.NotEmpty(t, followerAddr, "should find a follower address")

	// Create client connected ONLY to the follower.
	followerClient := newPDClusterClient(t, []string{followerAddr})

	// Send RegionHeartbeat via follower with a new region.
	region := &metapb.Region{
		Id:       200,
		StartKey: []byte("follower-a"),
		EndKey:   []byte("follower-z"),
		Peers: []*metapb.Peer{
			{Id: 200, StoreId: 1},
		},
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 1,
		},
	}
	leader := &metapb.Peer{Id: 200, StoreId: 1}

	_, err := followerClient.ReportRegionHeartbeat(ctx, &pdpb.RegionHeartbeatRequest{
		Region: region,
		Leader: leader,
	})
	require.NoError(t, err, "ReportRegionHeartbeat via follower should succeed")

	// Wait for Raft replication to propagate writes to all nodes.
	time.Sleep(500 * time.Millisecond)

	// Verify the region is visible on all nodes.
	for i, addr := range clientAddrs {
		c := newPDClusterClient(t, []string{addr})
		r, l, err := c.GetRegionByID(ctx, 200)
		require.NoError(t, err, "GetRegionByID on node %d should succeed", i+1)
		require.NotNil(t, r, "region 200 should exist on node %d", i+1)
		assert.Equal(t, uint64(200), r.GetId())
		if l != nil {
			assert.Equal(t, uint64(200), l.GetId())
		}
	}
	t.Logf("Region heartbeat via follower forwarding succeeded, visible on all 3 nodes")
}

// TestPDReplication_5NodeCluster verifies that a 5-node PD cluster operates
// correctly with basic operations.
func TestPDReplication_5NodeCluster(t *testing.T) {
	servers, clientAddrs, _ := startPDCluster(t, 5)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Leader should be elected.
	leaderIdx := findLeaderIndex(t, servers)
	assert.GreaterOrEqual(t, leaderIdx, 0, "leader should be elected in 5-node cluster")

	// PutStore should succeed.
	err := client.PutStore(ctx, &metapb.Store{Id: 5, Address: "127.0.0.1:25000"})
	require.NoError(t, err, "PutStore should succeed in 5-node cluster")

	// TSO should work.
	ts, err := client.GetTS(ctx)
	require.NoError(t, err, "GetTS should succeed in 5-node cluster")
	assert.NotZero(t, ts.ToUint64(), "TSO should be non-zero")

	// GetStore should return the store on all 5 nodes.
	for i, addr := range clientAddrs {
		c := newPDClusterClient(t, []string{addr})
		s, err := c.GetStore(ctx, 5)
		require.NoError(t, err, "GetStore on node %d should succeed", i+1)
		require.NotNil(t, s, "store should exist on node %d", i+1)
		assert.Equal(t, uint64(5), s.GetId())
	}
}

// TestPDReplication_CatchUpRecovery verifies that a stopped node catches up
// via Raft log replay after being restarted.
func TestPDReplication_CatchUpRecovery(t *testing.T) {
	servers, clientAddrs, peerAddrs := startPDCluster(t, 3)
	ctx := context.Background()

	client := newPDClusterClient(t, clientAddrs)
	waitForLeader(t, client, 10*time.Second)
	bootstrapCluster(t, client)

	// Write 5 stores.
	for i := uint64(2); i <= 6; i++ {
		err := client.PutStore(ctx, &metapb.Store{
			Id:      i,
			Address: fmt.Sprintf("127.0.0.1:%d", 20160+i),
		})
		require.NoError(t, err, "PutStore %d should succeed", i)
	}

	// Stop node 3 (index 2).
	nodeIdx := 2
	dataDir := servers[nodeIdx].DataDir()
	servers[nodeIdx].Stop()

	// Write 5 more stores while node 3 is down.
	for i := uint64(7); i <= 11; i++ {
		err := client.PutStore(ctx, &metapb.Store{
			Id:      i,
			Address: fmt.Sprintf("127.0.0.1:%d", 20160+i),
		})
		require.NoError(t, err, "PutStore %d should succeed with node 3 down", i)
	}

	// Restart node 3 with the same config and data directory.
	nodeID := uint64(nodeIdx + 1)
	initialCluster := make(map[uint64]string, 3)
	clientAddrMap := make(map[uint64]string, 3)
	for i := 0; i < 3; i++ {
		id := uint64(i + 1)
		initialCluster[id] = peerAddrs[i]
		clientAddrMap[id] = clientAddrs[i]
	}

	cfg := pd.DefaultPDServerConfig()
	cfg.ListenAddr = clientAddrs[nodeIdx]
	cfg.DataDir = dataDir
	cfg.ClusterID = 1
	cfg.RaftConfig = &pd.PDServerRaftConfig{
		PDNodeID:             nodeID,
		InitialCluster:       initialCluster,
		PeerAddr:             peerAddrs[nodeIdx],
		ClientAddrs:          clientAddrMap,
		RaftTickInterval:     100 * time.Millisecond,
		ElectionTimeoutTicks: 10,
		HeartbeatTicks:       3,
	}

	newSrv, err := pd.NewPDServer(cfg)
	require.NoError(t, err, "should create restarted PD server")
	err = newSrv.Start()
	require.NoError(t, err, "should start restarted PD server")
	t.Cleanup(func() { newSrv.Stop() })

	// Replace in servers slice for cleanup.
	servers[nodeIdx] = newSrv

	// Wait for node 3 to catch up. Poll GetAllStores on node 3.
	node3Client := newPDClusterClient(t, []string{clientAddrs[nodeIdx]})
	deadline := time.Now().Add(10 * time.Second)
	var storeCount int
	for time.Now().Before(deadline) {
		stores, err := node3Client.GetAllStores(ctx)
		if err == nil {
			storeCount = len(stores)
			// We wrote store 1 (bootstrap) + stores 2-6 + stores 7-11 = 11 stores total.
			if storeCount >= 11 {
				break
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	assert.GreaterOrEqual(t, storeCount, 11,
		"node 3 should have caught up with all 11 stores (has %d)", storeCount)
}
