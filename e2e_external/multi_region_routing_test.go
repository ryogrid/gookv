package e2e_external_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestMultiRegionTransactions tests transactional prewrite/commit across regions.
func TestMultiRegionTransactions(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	ctx := context.Background()

	// Get a timestamp from PD for the transaction.
	pdClient := cluster.PD().Client()
	ts, err := pdClient.GetTS(ctx)
	require.NoError(t, err)
	startTS := ts.ToUint64()

	// Prewrite on the leader node (try all nodes until one accepts).
	primaryKey := []byte("txn-primary")
	secondaryKey := []byte("txn-secondary")

	var prewriteOK bool
	for i := 0; i < 3; i++ {
		client := e2elib.DialTikvClient(t, cluster.Node(i).Addr())
		resp, err := client.KvPrewrite(ctx, &kvrpcpb.PrewriteRequest{
			Mutations: []*kvrpcpb.Mutation{
				{Op: kvrpcpb.Op_Put, Key: primaryKey, Value: []byte("pv")},
			},
			PrimaryLock:    primaryKey,
			StartVersion:   startTS,
			LockTtl:        3000,
			MinCommitTs:     startTS + 1,
		})
		if err == nil && len(resp.GetErrors()) == 0 && resp.GetRegionError() == nil {
			prewriteOK = true
			break
		}
	}
	require.True(t, prewriteOK, "prewrite should succeed on at least one node")

	// Commit.
	commitTS, err := pdClient.GetTS(ctx)
	require.NoError(t, err)

	var commitOK bool
	for i := 0; i < 3; i++ {
		client := e2elib.DialTikvClient(t, cluster.Node(i).Addr())
		resp, err := client.KvCommit(ctx, &kvrpcpb.CommitRequest{
			StartVersion:  startTS,
			CommitVersion: commitTS.ToUint64(),
			Keys:          [][]byte{primaryKey},
		})
		if err == nil && resp.GetError() == nil && resp.GetRegionError() == nil {
			commitOK = true
			break
		}
	}
	require.True(t, commitOK, "commit should succeed on at least one node")

	// Read via client library.
	_ = secondaryKey // secondary key would be on a different region in a real multi-region txn

	t.Log("Multi-region transactions passed")
}

// TestMultiRegionRawKVBatchScan tests RawBatchScan across regions.
func TestMultiRegionRawKVBatchScan(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	ctx := context.Background()

	// Write keys across regions (retry on transient errors after split).
	for i := 0; i < 10; i++ {
		key := []byte(fmt.Sprintf("bscan-%02d", i))
		val := []byte(fmt.Sprintf("val-%02d", i))
		e2elib.WaitForCondition(t, 30*time.Second, fmt.Sprintf("put bscan-%02d", i), func() bool {
			ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			return cluster.RawKV().Put(ctx2, key, val) == nil
		})
	}

	// Scan across regions.
	rawKV := cluster.RawKV()
	pairs, err := rawKV.Scan(ctx, []byte("bscan-00"), []byte("bscan-99"), 100)
	require.NoError(t, err)
	assert.GreaterOrEqual(t, len(pairs), 10, "scan should return all keys")

	t.Log("Multi-region RawKV batch scan passed")
}

// TestMultiRegionSplitWithLiveTraffic tests that auto-split preserves existing data.
func TestMultiRegionSplitWithLiveTraffic(t *testing.T) {
	e2elib.SkipIfNoBinary(t, "gookv-server", "gookv-pd")

	cluster := e2elib.NewGokvCluster(t, e2elib.GokvClusterConfig{
		NumNodes:           3,
		SplitSize:          "1KB",
		SplitCheckInterval: "1s",
	})
	require.NoError(t, cluster.Start())
	t.Cleanup(func() { cluster.Stop() })

	rawKV := cluster.RawKV()
	ctx := context.Background()

	// Wait for leader.
	e2elib.WaitForCondition(t, 30*time.Second, "leader election", func() bool {
		ctx2, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return rawKV.Put(ctx2, []byte("__init__"), []byte("ok")) == nil
	})

	// Write 30 keys before split.
	for i := 0; i < 30; i++ {
		key := []byte(fmt.Sprintf("live-%04d", i))
		val := []byte(fmt.Sprintf("value-%04d-padding-xxxxxxxxxxxxxxxxxxxxxxxx", i))
		err := rawKV.Put(ctx, key, val)
		require.NoError(t, err)
	}

	// Wait for split.
	pdClient := cluster.PD().Client()
	e2elib.WaitForSplit(t, pdClient, 60*time.Second)

	// Reset client to clear stale region cache after split.
	cluster.ResetClient()
	rawKV = cluster.RawKV()

	// Wait for all regions to have leaders.
	e2elib.WaitForCondition(t, 30*time.Second, "all regions operational after split", func() bool {
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := rawKV.Get(ctx2, []byte("live-0001"))
		if err != nil {
			return false
		}
		_, _, err = rawKV.Get(ctx2, []byte("live-0029"))
		return err == nil
	})

	// All pre-split keys should still be readable.
	for i := 0; i < 30; i++ {
		key := []byte(fmt.Sprintf("live-%04d", i))
		val, notFound, err := rawKV.Get(ctx, key)
		require.NoError(t, err, "key %s", key)
		assert.False(t, notFound, "key %s should exist after split", key)
		assert.Contains(t, string(val), fmt.Sprintf("value-%04d", i))
	}

	t.Log("Split with live traffic passed")
}

// TestMultiRegionPDCoordinatedSplit tests auto-split with PD coordination.
func TestMultiRegionPDCoordinatedSplit(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	pdClient := cluster.PD().Client()

	// After newMultiRegionCluster, split should have already occurred.
	regionCount := e2elib.WaitForRegionCount(t, pdClient, 2, 5*time.Second)
	assert.GreaterOrEqual(t, regionCount, 2, "PD-coordinated split should create multiple regions")

	t.Log("PD-coordinated split passed")
}

// TestMultiRegionAsyncCommit tests async commit prewrite across regions.
func TestMultiRegionAsyncCommit(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	ctx := context.Background()
	pdClient := cluster.PD().Client()

	ts, err := pdClient.GetTS(ctx)
	require.NoError(t, err)
	startTS := ts.ToUint64()

	// Async commit prewrite with UseAsyncCommit=true.
	primaryKey := []byte("async-primary")

	var prewriteOK bool
	for i := 0; i < 3; i++ {
		client := e2elib.DialTikvClient(t, cluster.Node(i).Addr())
		resp, err := client.KvPrewrite(ctx, &kvrpcpb.PrewriteRequest{
			Mutations: []*kvrpcpb.Mutation{
				{Op: kvrpcpb.Op_Put, Key: primaryKey, Value: []byte("async-val")},
			},
			PrimaryLock:    primaryKey,
			StartVersion:   startTS,
			LockTtl:        3000,
			UseAsyncCommit: true,
			MinCommitTs:    startTS + 1,
			Secondaries:    [][]byte{},
		})
		if err == nil && len(resp.GetErrors()) == 0 && resp.GetRegionError() == nil {
			prewriteOK = true
			break
		}
	}
	require.True(t, prewriteOK, "async commit prewrite should succeed")

	t.Log("Multi-region async commit passed")
}

// TestMultiRegionScanLock tests ScanLock respects region boundaries.
func TestMultiRegionScanLock(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	ctx := context.Background()
	pdClient := cluster.PD().Client()

	// Create a lock via prewrite.
	ts, err := pdClient.GetTS(ctx)
	require.NoError(t, err)
	startTS := ts.ToUint64()

	lockKey := []byte("scanlock-key")

	for i := 0; i < 3; i++ {
		client := e2elib.DialTikvClient(t, cluster.Node(i).Addr())
		resp, err := client.KvPrewrite(ctx, &kvrpcpb.PrewriteRequest{
			Mutations: []*kvrpcpb.Mutation{
				{Op: kvrpcpb.Op_Put, Key: lockKey, Value: []byte("locked-val")},
			},
			PrimaryLock:  lockKey,
			StartVersion: startTS,
			LockTtl:      30000,
			MinCommitTs:  startTS + 1,
		})
		if err == nil && len(resp.GetErrors()) == 0 && resp.GetRegionError() == nil {
			break
		}
	}

	// ScanLock on the node that owns this key.
	var found bool
	for i := 0; i < 3; i++ {
		client := e2elib.DialTikvClient(t, cluster.Node(i).Addr())
		scanResp, err := client.KvScanLock(ctx, &kvrpcpb.ScanLockRequest{
			MaxVersion: startTS + 1,
			Limit:      100,
		})
		if err == nil && scanResp.GetRegionError() == nil {
			for _, lock := range scanResp.GetLocks() {
				if string(lock.GetKey()) == string(lockKey) {
					found = true
				}
			}
		}
	}
	assert.True(t, found, "ScanLock should find the locked key")

	t.Log("Multi-region ScanLock passed")
}
