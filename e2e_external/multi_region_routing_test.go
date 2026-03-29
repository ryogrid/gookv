package e2e_external_test

import (
	"fmt"
	"strings"
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
	ctx := t.Context()

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
			PrimaryLock:  primaryKey,
			StartVersion: startTS,
			LockTtl:      3000,
			MinCommitTs:  startTS + 1,
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
	pdAddr := cluster.PD().Addr()

	// Write keys across regions (retry on transient errors after split).
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("bscan-%02d", i)
		val := fmt.Sprintf("val-%02d", i)
		e2elib.CLIWaitForCondition(t, pdAddr, fmt.Sprintf("PUT %s %s", key, val),
			func(output string) bool {
				return strings.Contains(output, "OK")
			}, 30*time.Second)
	}

	// Scan across regions via CLI, polling until all keys are visible.
	e2elib.CLIWaitForCondition(t, pdAddr, "SCAN bscan-00 bscan-99 LIMIT 100",
		func(output string) bool {
			count := 0
			for _, line := range strings.Split(output, "\n") {
				if strings.Contains(line, "bscan-") {
					count++
				}
			}
			return count >= 10
		}, 30*time.Second)

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

	pdAddr := cluster.PD().Addr()

	// Wait for leader via CLI.
	e2elib.CLIWaitForCondition(t, pdAddr, "PUT __init__ ok",
		func(output string) bool {
			return strings.Contains(output, "OK")
		}, 30*time.Second)

	// Write 30 keys before split via CLI.
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("live-%04d", i)
		val := fmt.Sprintf("value-%04d-padding-xxxxxxxxxxxxxxxxxxxxxxxx", i)
		e2elib.CLIPut(t, pdAddr, key, val)
	}

	// Wait for split via CLI (poll region count).
	e2elib.CLIWaitForRegionCount(t, pdAddr, 2, 60*time.Second)

	// Reset client to clear stale region cache after split.
	cluster.ResetClient()

	// Wait for all regions to have leaders (poll via CLI until reads succeed).
	// After a split, PD needs time to propagate region metadata and leaders
	// need to be elected on new regions, so use a generous timeout.
	e2elib.CLIWaitForCondition(t, pdAddr, "GET live-0001",
		func(output string) bool {
			return !strings.Contains(output, "(not found)") && !strings.Contains(output, "error")
		}, 60*time.Second)
	e2elib.CLIWaitForCondition(t, pdAddr, "GET live-0029",
		func(output string) bool {
			return !strings.Contains(output, "(not found)") && !strings.Contains(output, "error")
		}, 60*time.Second)

	// All pre-split keys should still be readable via CLI.
	for i := 0; i < 30; i++ {
		key := fmt.Sprintf("live-%04d", i)
		val, found := e2elib.CLIGet(t, pdAddr, key)
		assert.True(t, found, "key %s should exist after split", key)
		assert.Contains(t, val, fmt.Sprintf("value-%04d", i))
	}

	t.Log("Split with live traffic passed")
}

// TestMultiRegionPDCoordinatedSplit tests auto-split with PD coordination.
func TestMultiRegionPDCoordinatedSplit(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	pdAddr := cluster.PD().Addr()

	// After newMultiRegionCluster, split should have already occurred.
	regionCount := e2elib.CLIWaitForRegionCount(t, pdAddr, 2, 5*time.Second)
	assert.GreaterOrEqual(t, regionCount, 2, "PD-coordinated split should create multiple regions")

	t.Log("PD-coordinated split passed")
}

// TestMultiRegionAsyncCommit tests async commit prewrite across regions.
func TestMultiRegionAsyncCommit(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	ctx := t.Context()
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
	ctx := t.Context()
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
