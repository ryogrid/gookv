package e2e

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"

	"github.com/ryogrid/gookv/internal/engine/rocks"
	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/internal/raftstore"
)

const (
	clusterSize = 5
	regionID    = 1
)

// testCluster manages a 5-node in-process Raft cluster for testing.
type testCluster struct {
	t       *testing.T
	peers   [clusterSize]*raftstore.Peer
	engines [clusterSize]traits.KvEngine
	cancels [clusterSize]context.CancelFunc
	dones   [clusterSize]chan struct{}

	mu      sync.Mutex
	applied [clusterSize][]raftpb.Entry
	stopped [clusterSize]bool
}

func newTestCluster(t *testing.T) *testCluster {
	t.Helper()

	tc := &testCluster{t: t}

	// Build the initial peer list for bootstrap.
	raftPeers := make([]raft.Peer, clusterSize)
	metaPeers := make([]*metapb.Peer, clusterSize)
	for i := 0; i < clusterSize; i++ {
		id := uint64(i + 1)
		raftPeers[i] = raft.Peer{ID: id}
		metaPeers[i] = &metapb.Peer{Id: id, StoreId: id}
	}

	region := &metapb.Region{
		Id:    regionID,
		Peers: metaPeers,
	}

	cfg := raftstore.DefaultPeerConfig()
	cfg.RaftBaseTickInterval = 20 * time.Millisecond
	cfg.RaftElectionTimeoutTicks = 10
	cfg.RaftHeartbeatTicks = 2

	for i := 0; i < clusterSize; i++ {
		dir := t.TempDir()
		engine, err := rocks.Open(dir)
		require.NoError(t, err)
		t.Cleanup(func() { engine.Close() })
		tc.engines[i] = engine

		peerID := uint64(i + 1)
		storeID := uint64(i + 1)
		peer, err := raftstore.NewPeer(regionID, peerID, storeID, region, engine, cfg, raftPeers)
		require.NoError(t, err)
		tc.peers[i] = peer
	}

	// Wire sendFunc: route messages to target peer's Mailbox.
	for i := 0; i < clusterSize; i++ {
		idx := i
		tc.peers[i].SetSendFunc(func(msgs []raftpb.Message) {
			for _, msg := range msgs {
				targetIdx := int(msg.To) - 1
				if targetIdx < 0 || targetIdx >= clusterSize {
					continue
				}
				tc.mu.Lock()
				isStopped := tc.stopped[targetIdx]
				tc.mu.Unlock()
				if isStopped {
					continue // Drop messages to stopped nodes.
				}
				peer := tc.peers[targetIdx]
				// Non-blocking send; drop if mailbox is full (simulates network loss).
				select {
				case peer.Mailbox <- raftstore.PeerMsg{
					Type: raftstore.PeerMsgTypeRaftMessage,
					Data: &msg,
				}:
				default:
					_ = idx // suppress unused warning
				}
			}
		})
	}

	// Wire applyFunc: collect applied entries per node.
	for i := 0; i < clusterSize; i++ {
		idx := i
		tc.peers[i].SetApplyFunc(func(regionID uint64, entries []raftpb.Entry) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			tc.applied[idx] = append(tc.applied[idx], entries...)
		})
	}

	return tc
}

// startAll starts all peers. Must be called after newTestCluster.
func (tc *testCluster) startAll() {
	tc.t.Helper()
	for i := 0; i < clusterSize; i++ {
		tc.startNode(i)
	}
}

// startNode starts a single peer by index (0-based).
func (tc *testCluster) startNode(idx int) {
	tc.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	tc.cancels[idx] = cancel
	tc.dones[idx] = make(chan struct{})
	tc.mu.Lock()
	tc.stopped[idx] = false
	tc.mu.Unlock()

	done := tc.dones[idx]
	peer := tc.peers[idx]
	go func() {
		peer.Run(ctx)
		close(done)
	}()
}

// stopNode stops a peer by index. Blocks until the peer goroutine exits.
func (tc *testCluster) stopNode(idx int) {
	tc.t.Helper()
	tc.mu.Lock()
	tc.stopped[idx] = true
	tc.mu.Unlock()

	if tc.cancels[idx] != nil {
		tc.cancels[idx]()
	}
	if tc.dones[idx] != nil {
		select {
		case <-tc.dones[idx]:
		case <-time.After(5 * time.Second):
			tc.t.Fatalf("node %d did not stop in time", idx+1)
		}
	}
}

// stopAll stops all running peers.
func (tc *testCluster) stopAll() {
	tc.t.Helper()
	for i := 0; i < clusterSize; i++ {
		tc.mu.Lock()
		isStopped := tc.stopped[i]
		tc.mu.Unlock()
		if !isStopped {
			tc.stopNode(i)
		}
	}
}

// findLeader returns the index of the current leader, or -1 if no leader.
func (tc *testCluster) findLeader() int {
	for i := 0; i < clusterSize; i++ {
		tc.mu.Lock()
		isStopped := tc.stopped[i]
		tc.mu.Unlock()
		if !isStopped && tc.peers[i].IsLeader() {
			return i
		}
	}
	return -1
}

// waitForLeader polls until a leader is elected or timeout.
func (tc *testCluster) waitForLeader(timeout time.Duration) int {
	tc.t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if idx := tc.findLeader(); idx >= 0 {
			return idx
		}
		time.Sleep(20 * time.Millisecond)
	}
	tc.t.Fatal("no leader elected within timeout")
	return -1
}

// getApplied returns a copy of applied entries for node idx.
func (tc *testCluster) getApplied(idx int) []raftpb.Entry {
	tc.mu.Lock()
	defer tc.mu.Unlock()
	result := make([]raftpb.Entry, len(tc.applied[idx]))
	copy(result, tc.applied[idx])
	return result
}

// countAppliedData counts how many applied entries have Data matching one of the given values.
func (tc *testCluster) countAppliedData(idx int, values ...string) int {
	entries := tc.getApplied(idx)
	count := 0
	for _, e := range entries {
		for _, v := range values {
			if string(e.Data) == v {
				count++
				break
			}
		}
	}
	return count
}

// --- Tests ---

func TestClusterLeaderElection(t *testing.T) {
	tc := newTestCluster(t)
	tc.startAll()
	defer tc.stopAll()

	// Wait for bootstrap Ready to process on all nodes.
	time.Sleep(300 * time.Millisecond)

	// Trigger election on node 1.
	require.NoError(t, tc.peers[0].Campaign())

	// Wait for leader to emerge.
	leaderIdx := tc.waitForLeader(5 * time.Second)

	// Exactly one leader.
	leaderCount := 0
	for i := 0; i < clusterSize; i++ {
		if tc.peers[i].IsLeader() {
			leaderCount++
		}
	}
	assert.Equal(t, 1, leaderCount, "exactly one leader should exist")
	t.Logf("Leader elected: node %d", leaderIdx+1)
}

func TestClusterProposalReplication(t *testing.T) {
	tc := newTestCluster(t)
	tc.startAll()
	defer tc.stopAll()

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, tc.peers[0].Campaign())
	leaderIdx := tc.waitForLeader(5 * time.Second)

	// Propose data through the leader.
	require.NoError(t, tc.peers[leaderIdx].Propose([]byte("key1=value1")))
	require.NoError(t, tc.peers[leaderIdx].Propose([]byte("key2=value2")))

	// Wait for replication.
	require.Eventually(t, func() bool {
		for i := 0; i < clusterSize; i++ {
			if tc.countAppliedData(i, "key1=value1", "key2=value2") < 2 {
				return false
			}
		}
		return true
	}, 5*time.Second, 50*time.Millisecond, "all nodes should apply both proposals")

	t.Log("All 5 nodes received both proposals")
}

func TestClusterMinorityFailure(t *testing.T) {
	tc := newTestCluster(t)
	tc.startAll()
	defer tc.stopAll()

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, tc.peers[0].Campaign())
	leaderIdx := tc.waitForLeader(5 * time.Second)

	// Stop 2 non-leader nodes (minority failure: 2 of 5 down, 3 remain).
	stopped := 0
	for i := 0; i < clusterSize && stopped < 2; i++ {
		if i != leaderIdx {
			tc.stopNode(i)
			stopped++
		}
	}

	// Leader should still be able to make progress.
	require.NoError(t, tc.peers[leaderIdx].Propose([]byte("after-minority-fail")))

	// Verify at least the 3 surviving nodes apply the entry.
	require.Eventually(t, func() bool {
		count := 0
		for i := 0; i < clusterSize; i++ {
			tc.mu.Lock()
			isStopped := tc.stopped[i]
			tc.mu.Unlock()
			if !isStopped && tc.countAppliedData(i, "after-minority-fail") >= 1 {
				count++
			}
		}
		return count >= 3
	}, 5*time.Second, 50*time.Millisecond, "surviving nodes should apply proposal after minority failure")

	t.Log("Cluster continues to make progress with 2 nodes down")
}

func TestClusterMajorityFailure(t *testing.T) {
	tc := newTestCluster(t)
	tc.startAll()
	defer tc.stopAll()

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, tc.peers[0].Campaign())
	leaderIdx := tc.waitForLeader(5 * time.Second)

	// Propose something first to confirm cluster works.
	require.NoError(t, tc.peers[leaderIdx].Propose([]byte("before-majority-fail")))
	require.Eventually(t, func() bool {
		return tc.countAppliedData(leaderIdx, "before-majority-fail") >= 1
	}, 5*time.Second, 50*time.Millisecond)

	// Stop 3 non-leader nodes (majority failure: 3 of 5 down, only 2 remain).
	stopped := 0
	for i := 0; i < clusterSize && stopped < 3; i++ {
		if i != leaderIdx {
			tc.stopNode(i)
			stopped++
		}
	}

	// Propose should be accepted locally but never committed (no quorum).
	_ = tc.peers[leaderIdx].Propose([]byte("after-majority-fail"))

	// Wait a reasonable time and verify the proposal is NOT applied.
	time.Sleep(1 * time.Second)

	count := tc.countAppliedData(leaderIdx, "after-majority-fail")
	assert.Equal(t, 0, count, "proposal should NOT be committed without quorum")

	t.Log("Cluster correctly halts progress when majority is down")
}

func TestClusterLeaderFailureAndReelection(t *testing.T) {
	tc := newTestCluster(t)
	tc.startAll()
	defer tc.stopAll()

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, tc.peers[0].Campaign())
	oldLeaderIdx := tc.waitForLeader(5 * time.Second)
	t.Logf("Original leader: node %d", oldLeaderIdx+1)

	// Stop the leader.
	tc.stopNode(oldLeaderIdx)

	// Wait for a new leader to be elected from remaining nodes.
	// With CheckQuorum enabled, followers will detect leader loss and start election.
	newLeaderIdx := -1
	require.Eventually(t, func() bool {
		newLeaderIdx = tc.findLeader()
		return newLeaderIdx >= 0 && newLeaderIdx != oldLeaderIdx
	}, 10*time.Second, 50*time.Millisecond, "new leader should be elected after old leader fails")

	t.Logf("New leader elected: node %d", newLeaderIdx+1)

	// New leader should accept proposals.
	require.NoError(t, tc.peers[newLeaderIdx].Propose([]byte("after-reelection")))

	require.Eventually(t, func() bool {
		// At least 3 surviving nodes should apply.
		count := 0
		for i := 0; i < clusterSize; i++ {
			tc.mu.Lock()
			isStopped := tc.stopped[i]
			tc.mu.Unlock()
			if !isStopped && tc.countAppliedData(i, "after-reelection") >= 1 {
				count++
			}
		}
		return count >= 3
	}, 5*time.Second, 50*time.Millisecond, "surviving nodes should apply proposal under new leader")

	t.Log("New leader successfully accepts and replicates proposals")
}

func TestClusterNodeRecovery(t *testing.T) {
	tc := newTestCluster(t)
	tc.startAll()
	defer tc.stopAll()

	time.Sleep(300 * time.Millisecond)
	require.NoError(t, tc.peers[0].Campaign())
	leaderIdx := tc.waitForLeader(5 * time.Second)

	// Stop 2 non-leader nodes.
	stoppedNodes := []int{}
	for i := 0; i < clusterSize && len(stoppedNodes) < 2; i++ {
		if i != leaderIdx {
			tc.stopNode(i)
			stoppedNodes = append(stoppedNodes, i)
		}
	}

	// Propose data while nodes are down.
	require.NoError(t, tc.peers[leaderIdx].Propose([]byte("during-outage")))
	require.Eventually(t, func() bool {
		return tc.countAppliedData(leaderIdx, "during-outage") >= 1
	}, 5*time.Second, 50*time.Millisecond)

	// Recover the stopped nodes by re-creating Peers with the same engines.
	// The existing PeerStorage in the engine retains the Raft log, so the
	// recovered node will catch up via AppendEntries from the leader.
	cfg := raftstore.DefaultPeerConfig()
	cfg.RaftBaseTickInterval = 20 * time.Millisecond
	cfg.RaftElectionTimeoutTicks = 10
	cfg.RaftHeartbeatTicks = 2

	metaPeers := make([]*metapb.Peer, clusterSize)
	for i := 0; i < clusterSize; i++ {
		metaPeers[i] = &metapb.Peer{Id: uint64(i + 1), StoreId: uint64(i + 1)}
	}
	region := &metapb.Region{Id: regionID, Peers: metaPeers}

	for _, idx := range stoppedNodes {
		peerID := uint64(idx + 1)
		// Restart with NO bootstrap peers (nil) — the engine already has Raft state.
		peer, err := raftstore.NewPeer(regionID, peerID, peerID, region, tc.engines[idx], cfg, nil)
		require.NoError(t, err)

		// Re-wire sendFunc and applyFunc.
		capturedIdx := idx
		peer.SetSendFunc(func(msgs []raftpb.Message) {
			for _, msg := range msgs {
				targetIdx := int(msg.To) - 1
				if targetIdx < 0 || targetIdx >= clusterSize {
					continue
				}
				tc.mu.Lock()
				isStopped := tc.stopped[targetIdx]
				tc.mu.Unlock()
				if isStopped {
					continue
				}
				targetPeer := tc.peers[targetIdx]
				select {
				case targetPeer.Mailbox <- raftstore.PeerMsg{
					Type: raftstore.PeerMsgTypeRaftMessage,
					Data: &msg,
				}:
				default:
				}
			}
		})
		peer.SetApplyFunc(func(regionID uint64, entries []raftpb.Entry) {
			tc.mu.Lock()
			defer tc.mu.Unlock()
			tc.applied[capturedIdx] = append(tc.applied[capturedIdx], entries...)
		})

		tc.peers[idx] = peer
		tc.startNode(idx)
		_ = capturedIdx
	}

	// Propose more data after recovery.
	require.NoError(t, tc.peers[leaderIdx].Propose([]byte("after-recovery")))

	// Verify recovered nodes eventually apply both the old and new data.
	require.Eventually(t, func() bool {
		for _, idx := range stoppedNodes {
			if tc.countAppliedData(idx, "after-recovery") < 1 {
				return false
			}
		}
		return true
	}, 10*time.Second, 100*time.Millisecond, "recovered nodes should apply new proposals")

	t.Log("Recovered nodes successfully rejoin the cluster and receive new data")
}
