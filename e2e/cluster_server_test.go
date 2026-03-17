package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/raft/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ryogrid/gookvs/internal/engine/rocks"
	"github.com/ryogrid/gookvs/internal/raftstore"
	raftrouter "github.com/ryogrid/gookvs/internal/raftstore/router"
	"github.com/ryogrid/gookvs/internal/server"
	"github.com/ryogrid/gookvs/internal/server/transport"
)

const serverClusterSize = 5

// serverNode represents a single gookvs-server node in the test cluster.
type serverNode struct {
	storeID     uint64
	srv         *server.Server
	coord       *server.StoreCoordinator
	raftClient  *transport.RaftClient
	addr        string
	cancel      context.CancelFunc
}

// testServerCluster manages a 5-node gookvs-server cluster for e2e testing.
type testServerCluster struct {
	t        *testing.T
	nodes    [serverClusterSize]*serverNode
	resolver *server.StaticStoreResolver
}

func newTestServerCluster(t *testing.T) *testServerCluster {
	t.Helper()

	// Pre-allocate the resolver with placeholder addresses.
	addrMap := make(map[uint64]string)
	for i := 0; i < serverClusterSize; i++ {
		addrMap[uint64(i+1)] = "" // Will be filled after server starts.
	}
	resolver := server.NewStaticStoreResolver(addrMap)

	tsc := &testServerCluster{
		t:        t,
		resolver: resolver,
	}

	// Build region metadata.
	metaPeers := make([]*metapb.Peer, serverClusterSize)
	raftPeers := make([]raft.Peer, serverClusterSize)
	for i := 0; i < serverClusterSize; i++ {
		id := uint64(i + 1)
		metaPeers[i] = &metapb.Peer{Id: id, StoreId: id}
		raftPeers[i] = raft.Peer{ID: id}
	}
	region := &metapb.Region{
		Id:    1,
		Peers: metaPeers,
	}

	// Create each node.
	for i := 0; i < serverClusterSize; i++ {
		storeID := uint64(i + 1)

		dir := t.TempDir()
		engine, err := rocks.Open(dir)
		require.NoError(t, err)
		t.Cleanup(func() { engine.Close() })

		storage := server.NewStorage(engine)
		srvCfg := server.ServerConfig{
			ListenAddr: "127.0.0.1:0",
		}
		srv := server.NewServer(srvCfg, storage)

		raftClient := transport.NewRaftClient(resolver, transport.DefaultRaftClientConfig())
		rtr := raftrouter.New(256)

		peerCfg := raftstore.DefaultPeerConfig()
		peerCfg.RaftBaseTickInterval = 20 * time.Millisecond
		peerCfg.RaftElectionTimeoutTicks = 10
		peerCfg.RaftHeartbeatTicks = 2

		coord := server.NewStoreCoordinator(server.StoreCoordinatorConfig{
			StoreID: storeID,
			Engine:  engine,
			Storage: storage,
			Router:  rtr,
			Client:  raftClient,
			PeerCfg: peerCfg,
		})
		srv.SetCoordinator(coord)

		// Bootstrap the region.
		require.NoError(t, coord.BootstrapRegion(region, raftPeers))

		// Start the gRPC server.
		require.NoError(t, srv.Start())
		addr := srv.Addr()

		// Update resolver with actual address.
		resolver.UpdateAddr(storeID, addr)

		tsc.nodes[i] = &serverNode{
			storeID:    storeID,
			srv:        srv,
			coord:      coord,
			raftClient: raftClient,
			addr:       addr,
		}
	}

	t.Cleanup(func() {
		tsc.stopAll()
	})

	return tsc
}

// stopAll stops all nodes.
func (tsc *testServerCluster) stopAll() {
	for i := 0; i < serverClusterSize; i++ {
		if tsc.nodes[i] != nil {
			tsc.stopNode(i)
		}
	}
}

// stopNode stops a single node by index (0-based).
func (tsc *testServerCluster) stopNode(idx int) {
	tsc.t.Helper()
	node := tsc.nodes[idx]
	if node == nil {
		return
	}
	node.coord.Stop()
	node.srv.Stop()
	node.raftClient.Close()
}

// dialNode creates a gRPC client connection to the node at the given index.
func (tsc *testServerCluster) dialNode(idx int) (*grpc.ClientConn, tikvpb.TikvClient) {
	tsc.t.Helper()
	node := tsc.nodes[idx]
	conn, err := grpc.Dial(node.addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(tsc.t, err)
	tsc.t.Cleanup(func() { conn.Close() })
	return conn, tikvpb.NewTikvClient(conn)
}

// findLeaderIdx finds the index of the Raft leader for region 1.
func (tsc *testServerCluster) findLeaderIdx() int {
	for i := 0; i < serverClusterSize; i++ {
		if tsc.nodes[i] == nil {
			continue
		}
		peer := tsc.nodes[i].coord.GetPeer(1)
		if peer != nil && peer.IsLeader() {
			return i
		}
	}
	return -1
}

// waitForLeader waits until a leader is elected.
func (tsc *testServerCluster) waitForLeader(timeout time.Duration) int {
	tsc.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if idx := tsc.findLeaderIdx(); idx >= 0 {
			return idx
		}
		time.Sleep(20 * time.Millisecond)
	}
	tsc.t.Fatal("no leader elected within timeout")
	return -1
}

// --- Server Cluster Tests ---

func TestClusterServerLeaderElection(t *testing.T) {
	tsc := newTestServerCluster(t)

	// Wait for bootstrap and natural election.
	time.Sleep(500 * time.Millisecond)

	// Trigger campaign on node 0 if no leader yet.
	if tsc.findLeaderIdx() < 0 {
		peer := tsc.nodes[0].coord.GetPeer(1)
		require.NotNil(t, peer)
		require.NoError(t, peer.Campaign())
	}

	leaderIdx := tsc.waitForLeader(10 * time.Second)
	t.Logf("Leader elected: node %d (store %d)", leaderIdx+1, tsc.nodes[leaderIdx].storeID)

	// Verify exactly one leader.
	leaderCount := 0
	for i := 0; i < serverClusterSize; i++ {
		peer := tsc.nodes[i].coord.GetPeer(1)
		if peer != nil && peer.IsLeader() {
			leaderCount++
		}
	}
	assert.Equal(t, 1, leaderCount, "exactly one leader")
}

func TestClusterServerKvOperations(t *testing.T) {
	tsc := newTestServerCluster(t)

	time.Sleep(500 * time.Millisecond)
	if tsc.findLeaderIdx() < 0 {
		peer := tsc.nodes[0].coord.GetPeer(1)
		require.NotNil(t, peer)
		require.NoError(t, peer.Campaign())
	}
	leaderIdx := tsc.waitForLeader(10 * time.Second)

	// Connect to the leader node via gRPC.
	_, client := tsc.dialNode(leaderIdx)
	ctx := context.Background()

	// Prewrite a key.
	prewriteResp, err := client.KvPrewrite(ctx, &kvrpcpb.PrewriteRequest{
		Mutations: []*kvrpcpb.Mutation{
			{Op: kvrpcpb.Op_Put, Key: []byte("k1"), Value: []byte("v1")},
		},
		PrimaryLock:  []byte("k1"),
		StartVersion: 10,
		LockTtl:      5000,
	})
	require.NoError(t, err)
	assert.Empty(t, prewriteResp.GetErrors(), "prewrite should succeed")

	// Commit the key.
	commitResp, err := client.KvCommit(ctx, &kvrpcpb.CommitRequest{
		Keys:          [][]byte{[]byte("k1")},
		StartVersion:  10,
		CommitVersion: 20,
	})
	require.NoError(t, err)
	assert.Nil(t, commitResp.GetError(), "commit should succeed")

	// Read the key back.
	getResp, err := client.KvGet(ctx, &kvrpcpb.GetRequest{
		Key:     []byte("k1"),
		Version: 25,
	})
	require.NoError(t, err)
	assert.False(t, getResp.GetNotFound(), "key should be found")
	assert.Equal(t, []byte("v1"), getResp.GetValue())

	t.Log("KV operations (prewrite → commit → get) succeeded on leader node")
}

func TestClusterServerCrossNodeReplication(t *testing.T) {
	tsc := newTestServerCluster(t)

	time.Sleep(500 * time.Millisecond)
	if tsc.findLeaderIdx() < 0 {
		peer := tsc.nodes[0].coord.GetPeer(1)
		require.NotNil(t, peer)
		require.NoError(t, peer.Campaign())
	}
	leaderIdx := tsc.waitForLeader(10 * time.Second)
	t.Logf("Leader: node %d", leaderIdx+1)

	// Write data via the leader node.
	_, leaderClient := tsc.dialNode(leaderIdx)
	ctx := context.Background()

	prewriteResp, err := leaderClient.KvPrewrite(ctx, &kvrpcpb.PrewriteRequest{
		Mutations: []*kvrpcpb.Mutation{
			{Op: kvrpcpb.Op_Put, Key: []byte("cross-node-key"), Value: []byte("replicated-value")},
		},
		PrimaryLock:  []byte("cross-node-key"),
		StartVersion: 50,
		LockTtl:      5000,
	})
	require.NoError(t, err)
	assert.Empty(t, prewriteResp.GetErrors(), "prewrite should succeed")

	commitResp, err := leaderClient.KvCommit(ctx, &kvrpcpb.CommitRequest{
		Keys:          [][]byte{[]byte("cross-node-key")},
		StartVersion:  50,
		CommitVersion: 60,
	})
	require.NoError(t, err)
	assert.Nil(t, commitResp.GetError(), "commit should succeed")

	// Wait for Raft replication to propagate to all nodes.
	time.Sleep(500 * time.Millisecond)

	// Read from a DIFFERENT node (not the leader).
	followerIdx := -1
	for i := 0; i < serverClusterSize; i++ {
		if i != leaderIdx {
			followerIdx = i
			break
		}
	}
	require.NotEqual(t, -1, followerIdx)

	_, followerClient := tsc.dialNode(followerIdx)
	getResp, err := followerClient.KvGet(ctx, &kvrpcpb.GetRequest{
		Key:     []byte("cross-node-key"),
		Version: 100,
	})
	require.NoError(t, err)
	assert.False(t, getResp.GetNotFound(), "key should be found on follower node")
	assert.Equal(t, []byte("replicated-value"), getResp.GetValue(),
		"value on follower should match what was written via leader")

	t.Logf("Cross-node replication verified: wrote on node %d, read from node %d",
		leaderIdx+1, followerIdx+1)
}

func TestClusterServerNodeFailure(t *testing.T) {
	tsc := newTestServerCluster(t)

	time.Sleep(500 * time.Millisecond)
	if tsc.findLeaderIdx() < 0 {
		peer := tsc.nodes[0].coord.GetPeer(1)
		require.NotNil(t, peer)
		require.NoError(t, peer.Campaign())
	}
	leaderIdx := tsc.waitForLeader(10 * time.Second)
	t.Logf("Initial leader: node %d", leaderIdx+1)

	// Write some data first.
	_, client := tsc.dialNode(leaderIdx)
	ctx := context.Background()

	_, err := client.KvPrewrite(ctx, &kvrpcpb.PrewriteRequest{
		Mutations:    []*kvrpcpb.Mutation{{Op: kvrpcpb.Op_Put, Key: []byte("before-fail"), Value: []byte("ok")}},
		PrimaryLock:  []byte("before-fail"),
		StartVersion: 100,
		LockTtl:      5000,
	})
	require.NoError(t, err)
	_, err = client.KvCommit(ctx, &kvrpcpb.CommitRequest{
		Keys: [][]byte{[]byte("before-fail")}, StartVersion: 100, CommitVersion: 110,
	})
	require.NoError(t, err)

	// Stop 2 non-leader nodes (minority failure).
	stopped := 0
	for i := 0; i < serverClusterSize && stopped < 2; i++ {
		if i != leaderIdx {
			tsc.stopNode(i)
			tsc.nodes[i] = nil
			stopped++
			t.Logf("Stopped node %d", i+1)
		}
	}

	// Leader should still be operational for reads.
	getResp, err := client.KvGet(ctx, &kvrpcpb.GetRequest{
		Key: []byte("before-fail"), Version: 200,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("ok"), getResp.GetValue())

	t.Log("Cluster survives minority node failure, reads still work on leader")
}

func TestClusterServerLeaderFailover(t *testing.T) {
	tsc := newTestServerCluster(t)

	time.Sleep(500 * time.Millisecond)
	if tsc.findLeaderIdx() < 0 {
		peer := tsc.nodes[0].coord.GetPeer(1)
		require.NotNil(t, peer)
		require.NoError(t, peer.Campaign())
	}
	oldLeaderIdx := tsc.waitForLeader(10 * time.Second)
	t.Logf("Original leader: node %d", oldLeaderIdx+1)

	// Stop the leader.
	tsc.stopNode(oldLeaderIdx)
	tsc.nodes[oldLeaderIdx] = nil
	t.Logf("Stopped leader node %d", oldLeaderIdx+1)

	// Wait for new leader election.
	var newLeaderIdx int
	require.Eventually(t, func() bool {
		newLeaderIdx = tsc.findLeaderIdx()
		return newLeaderIdx >= 0 && newLeaderIdx != oldLeaderIdx
	}, 10*time.Second, 50*time.Millisecond, "new leader should be elected")

	t.Logf("New leader: node %d", newLeaderIdx+1)

	// Verify the new leader accepts gRPC KV operations.
	_, client := tsc.dialNode(newLeaderIdx)
	ctx := context.Background()

	prewriteResp, err := client.KvPrewrite(ctx, &kvrpcpb.PrewriteRequest{
		Mutations:    []*kvrpcpb.Mutation{{Op: kvrpcpb.Op_Put, Key: []byte("after-failover"), Value: []byte("works")}},
		PrimaryLock:  []byte("after-failover"),
		StartVersion: 300,
		LockTtl:      5000,
	})
	require.NoError(t, err)
	assert.Empty(t, prewriteResp.GetErrors())

	commitResp, err := client.KvCommit(ctx, &kvrpcpb.CommitRequest{
		Keys: [][]byte{[]byte("after-failover")}, StartVersion: 300, CommitVersion: 310,
	})
	require.NoError(t, err)
	assert.Nil(t, commitResp.GetError())

	getResp, err := client.KvGet(ctx, &kvrpcpb.GetRequest{
		Key: []byte("after-failover"), Version: 400,
	})
	require.NoError(t, err)
	assert.Equal(t, []byte("works"), getResp.GetValue())

	fmt.Println("Leader failover succeeded: new leader accepts KV operations")
}
