package e2e_external_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// newMultiRegionCluster creates a 3-node cluster with small split size to trigger auto-split.
// Writes enough data to cause at least one split, then waits for split to complete.
func newMultiRegionCluster(t *testing.T) *e2elib.GokvCluster {
	t.Helper()
	e2elib.SkipIfNoBinary(t, "gookv-server", "gookv-pd")

	cluster := e2elib.NewGokvCluster(t, e2elib.GokvClusterConfig{
		NumNodes:           3,
		SplitSize:          "1KB",
		SplitCheckInterval: "1s",
	})
	require.NoError(t, cluster.Start())
	t.Cleanup(func() { cluster.Stop() })

	rawKV := cluster.RawKV()

	// Wait for leader election.
	e2elib.WaitForCondition(t, 30*time.Second, "leader election", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return rawKV.Put(ctx, []byte("__init__"), []byte("ok")) == nil
	})

	// Write enough data to trigger at least one split (>1KB).
	// Use retry for each Put since a split may occur mid-write, causing
	// transient "not leader" errors until PD and region cache catch up.
	ctx := context.Background()
	for i := 0; i < 50; i++ {
		key := []byte(fmt.Sprintf("split-seed-%04d", i))
		val := []byte(fmt.Sprintf("value-%04d-padding-to-make-it-bigger-%s", i, "xxxxxxxxxxxxxxxxxxxxxxxx"))
		retried := 0
		e2elib.WaitForCondition(t, 60*time.Second, fmt.Sprintf("put split-seed-%04d", i), func() bool {
			ctx2, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			retried++
			if retried%5 == 0 {
				// Periodically reset client to clear stale region cache.
				cluster.ResetClient()
			}
			err := cluster.RawKV().Put(ctx2, key, val)
			return err == nil
		})
	}

	// Wait for split to occur.
	pdClient := cluster.PD().Client()
	e2elib.WaitForSplit(t, pdClient, 60*time.Second)

	// Reset client to clear stale region cache after split.
	cluster.ResetClient()
	rawKV = cluster.RawKV()

	// Wait for leaders on all new regions by verifying reads work across key space.
	e2elib.WaitForCondition(t, 60*time.Second, "all regions have leaders after split", func() bool {
		ctx2, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _, err := rawKV.Get(ctx2, []byte("split-seed-0001"))
		if err != nil {
			return false
		}
		_, _, err = rawKV.Get(ctx2, []byte("split-seed-0049"))
		return err == nil
	})

	return cluster
}

// TestMultiRegionKeyRouting verifies keys route to the correct region.
func TestMultiRegionKeyRouting(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	pdAddr := cluster.PD().Addr()

	// After split, different keys should be in different regions.
	out1 := e2elib.CLIExec(t, pdAddr, "REGION split-seed-0001")
	assert.True(t, strings.Contains(out1, "Region ID:"), "REGION output should contain Region ID")

	out2 := e2elib.CLIExec(t, pdAddr, "REGION split-seed-0049")
	assert.True(t, strings.Contains(out2, "Region ID:"), "REGION output should contain Region ID")

	// If split happened, at least 2 regions should exist.
	regionCount := e2elib.CLIWaitForRegionCount(t, pdAddr, 2, 5*time.Second)
	assert.GreaterOrEqual(t, regionCount, 2, "should have at least 2 regions after split")

	t.Log("Multi-region key routing passed")
}

// TestMultiRegionIndependentLeaders verifies PD tracks leaders per region.
func TestMultiRegionIndependentLeaders(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	pdAddr := cluster.PD().Addr()

	// Wait for leaders on both regions.
	leaderStore1 := e2elib.CLIWaitForRegionLeader(t, pdAddr, "split-seed-0001", 30*time.Second)
	leaderStore2 := e2elib.CLIWaitForRegionLeader(t, pdAddr, "split-seed-0049", 30*time.Second)

	// Both should have leaders (may or may not be the same store).
	assert.NotZero(t, leaderStore1, "region 1 should have a leader")
	assert.NotZero(t, leaderStore2, "region 2 should have a leader")

	t.Log("Multi-region independent leaders passed")
}

// TestMultiRegionRawKV tests RawPut/RawGet across multiple regions.
func TestMultiRegionRawKV(t *testing.T) {
	cluster := newMultiRegionCluster(t)
	pdAddr := cluster.PD().Addr()

	// Write keys that should span multiple regions.
	// Use CLIWaitForCondition for each Put since new regions after split may still be electing leaders.
	keys := []string{"aaa-key", "mmm-key", "zzz-key"}
	for i, k := range keys {
		valStr := fmt.Sprintf("val-%d", i)
		e2elib.CLIWaitForCondition(t, pdAddr, fmt.Sprintf("PUT %s %s", k, valStr),
			func(output string) bool {
				return strings.Contains(output, "OK")
			}, 30*time.Second)
	}

	// Read back all keys.
	for i, k := range keys {
		val, found := e2elib.CLIGet(t, pdAddr, k)
		assert.True(t, found, "key %s should exist", k)
		assert.Equal(t, fmt.Sprintf("val-%d", i), val)
	}

	t.Log("Multi-region RawKV passed")
}
