package e2e

import (
	"context"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/raft/v3"

	"github.com/ryogrid/gookvs/internal/engine/rocks"
	"github.com/ryogrid/gookvs/internal/raftstore"
	raftrouter "github.com/ryogrid/gookvs/internal/raftstore/router"
	"github.com/ryogrid/gookvs/internal/server"
	"github.com/ryogrid/gookvs/internal/server/transport"
	"github.com/ryogrid/gookvs/pkg/pdclient"
)

const pdClusterSize = 3

// pdClusterNode represents a node in a PD-integrated test cluster.
type pdClusterNode struct {
	storeID    uint64
	srv        *server.Server
	coord      *server.StoreCoordinator
	pdWorker   *server.PDWorker
	raftClient *transport.RaftClient
	addr       string
}

// newPDIntegratedCluster creates a 3-node cluster with PD integration.
// Peers send region heartbeats to PD so leader info is available.
func newPDIntegratedCluster(t *testing.T) ([]*pdClusterNode, pdclient.Client, string) {
	t.Helper()

	// Start PD server.
	_, pdAddr := startPDServer(t)
	pdClient := newPDClient(t, pdAddr)

	// Pre-allocate resolver.
	addrMap := make(map[uint64]string)
	for i := 0; i < pdClusterSize; i++ {
		addrMap[uint64(i+1)] = ""
	}
	resolver := server.NewStaticStoreResolver(addrMap)

	// Build region metadata.
	metaPeers := make([]*metapb.Peer, pdClusterSize)
	raftPeers := make([]raft.Peer, pdClusterSize)
	for i := 0; i < pdClusterSize; i++ {
		id := uint64(i + 1)
		metaPeers[i] = &metapb.Peer{Id: id, StoreId: id}
		raftPeers[i] = raft.Peer{ID: id}
	}
	region := &metapb.Region{Id: 1, Peers: metaPeers}

	// Bootstrap cluster in PD.
	ctx := context.Background()
	store1 := &metapb.Store{Id: 1, Address: "placeholder"}
	_, err := pdClient.Bootstrap(ctx, store1, region)
	require.NoError(t, err)

	nodes := make([]*pdClusterNode, pdClusterSize)

	for i := 0; i < pdClusterSize; i++ {
		storeID := uint64(i + 1)

		dir := t.TempDir()
		engine, err := rocks.Open(dir)
		require.NoError(t, err)
		t.Cleanup(func() { engine.Close() })

		storage := server.NewStorage(engine)
		srvCfg := server.ServerConfig{ListenAddr: "127.0.0.1:0"}
		srv := server.NewServer(srvCfg, storage)

		raftClient := transport.NewRaftClient(resolver, transport.DefaultRaftClientConfig())
		rtr := raftrouter.New(256)

		peerCfg := raftstore.DefaultPeerConfig()
		peerCfg.RaftBaseTickInterval = 20 * time.Millisecond
		peerCfg.RaftElectionTimeoutTicks = 10
		peerCfg.RaftHeartbeatTicks = 2

		// Create PDWorker and get task channel.
		nodePDClient := newPDClient(t, pdAddr)
		pdWorker := server.NewPDWorker(server.PDWorkerConfig{
			StoreID:  storeID,
			PDClient: nodePDClient,
		})

		coord := server.NewStoreCoordinator(server.StoreCoordinatorConfig{
			StoreID:  storeID,
			Engine:   engine,
			Storage:  storage,
			Router:   rtr,
			Client:   raftClient,
			PeerCfg:  peerCfg,
			PDTaskCh: pdWorker.PeerTaskCh(),
		})
		srv.SetCoordinator(coord)
		pdWorker.SetCoordinator(coord)
		pdWorker.Run()

		require.NoError(t, coord.BootstrapRegion(region, raftPeers))
		require.NoError(t, srv.Start())
		addr := srv.Addr()
		resolver.UpdateAddr(storeID, addr)

		// Register store with PD (with actual address).
		require.NoError(t, pdClient.PutStore(ctx, &metapb.Store{Id: storeID, Address: addr}))

		nodes[i] = &pdClusterNode{
			storeID:    storeID,
			srv:        srv,
			coord:      coord,
			pdWorker:   pdWorker,
			raftClient: raftClient,
			addr:       addr,
		}
	}

	t.Cleanup(func() {
		for _, n := range nodes {
			if n != nil {
				n.pdWorker.Stop()
				n.coord.Stop()
				n.srv.Stop()
				n.raftClient.Close()
			}
		}
	})

	return nodes, pdClient, pdAddr
}

// waitForPDLeader polls PD until it reports a leader for region 1.
func waitForPDLeader(t *testing.T, pdClient pdclient.Client, timeout time.Duration) (uint64, *metapb.Peer) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, leader, err := pdClient.GetRegion(ctx, []byte("test-key"))
		cancel()
		if err == nil && leader != nil && leader.GetStoreId() != 0 {
			return leader.GetStoreId(), leader
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("timed out waiting for PD to report a leader")
	return 0, nil
}

// TestPDStoreRegistration verifies all stores are registered with PD and have correct addresses.
func TestPDStoreRegistration(t *testing.T) {
	nodes, pdClient, _ := newPDIntegratedCluster(t)
	ctx := context.Background()

	for _, n := range nodes {
		store, err := pdClient.GetStore(ctx, n.storeID)
		require.NoError(t, err)
		assert.Equal(t, n.storeID, store.GetId())
		assert.Equal(t, n.addr, store.GetAddress(), "store %d address mismatch", n.storeID)
	}
	t.Log("All stores registered with PD with correct addresses")
}

// TestPDRegionLeaderTracking verifies that PD knows the current leader after election.
func TestPDRegionLeaderTracking(t *testing.T) {
	nodes, pdClient, _ := newPDIntegratedCluster(t)

	leaderStoreID, leader := waitForPDLeader(t, pdClient, 30*time.Second)
	assert.NotZero(t, leaderStoreID)
	assert.NotNil(t, leader)

	// Verify the leader store exists in our cluster.
	found := false
	for _, n := range nodes {
		if n.storeID == leaderStoreID {
			found = true
			break
		}
	}
	assert.True(t, found, "leader store %d should be in the cluster", leaderStoreID)
	t.Logf("PD reports leader: store %d (peer %d)", leaderStoreID, leader.GetId())
}

// TestPDLeaderFailover verifies that after killing the leader, PD updates to the new leader.
func TestPDLeaderFailover(t *testing.T) {
	nodes, pdClient, _ := newPDIntegratedCluster(t)

	// Wait for initial leader.
	oldLeaderStoreID, _ := waitForPDLeader(t, pdClient, 30*time.Second)
	t.Logf("Initial leader: store %d", oldLeaderStoreID)

	// Find and stop the leader node.
	for i, n := range nodes {
		if n.storeID == oldLeaderStoreID {
			t.Logf("Stopping leader node %d (store %d)", i, n.storeID)
			n.pdWorker.Stop()
			n.coord.Stop()
			n.srv.Stop()
			n.raftClient.Close()
			nodes[i] = nil // Mark as stopped.
			break
		}
	}

	// Wait for a new leader to be reported by PD.
	deadline := time.Now().Add(30 * time.Second)
	var newLeaderStoreID uint64
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, leader, err := pdClient.GetRegion(ctx, []byte("test-key"))
		cancel()
		if err == nil && leader != nil && leader.GetStoreId() != 0 && leader.GetStoreId() != oldLeaderStoreID {
			newLeaderStoreID = leader.GetStoreId()
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	require.NotZero(t, newLeaderStoreID, "PD should report a new leader after failover")
	assert.NotEqual(t, oldLeaderStoreID, newLeaderStoreID)
	t.Logf("New leader after failover: store %d", newLeaderStoreID)

	// Verify the new leader's store address is correct and the node is reachable.
	ctx := context.Background()
	store, err := pdClient.GetStore(ctx, newLeaderStoreID)
	require.NoError(t, err)
	require.NotEmpty(t, store.GetAddress())

	// Find the live node and verify it matches.
	for _, n := range nodes {
		if n != nil && n.storeID == newLeaderStoreID {
			assert.Equal(t, n.addr, store.GetAddress())
			t.Logf("New leader reachable at %s", store.GetAddress())
			break
		}
	}
}
