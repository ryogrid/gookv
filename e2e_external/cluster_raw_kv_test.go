package e2e_external_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// newClusterWithLeader creates a 3-node cluster and waits for Raft leader election.
func newClusterWithLeader(t *testing.T) *e2elib.GokvCluster {
	t.Helper()
	e2elib.SkipIfNoBinary(t, "gookv-server", "gookv-pd")

	cluster := e2elib.NewGokvCluster(t, e2elib.GokvClusterConfig{NumNodes: 3})
	require.NoError(t, cluster.Start())
	t.Cleanup(func() { cluster.Stop() })

	// Wait for Raft leader election.
	rawKV := cluster.RawKV()
	e2elib.WaitForCondition(t, 30*time.Second, "cluster leader election", func() bool {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		return rawKV.Put(ctx, []byte("__health__"), []byte("ok")) == nil
	})

	return cluster
}

// TestClusterRawKVOperations tests RawPut, RawGet, RawDelete via Raft consensus.
func TestClusterRawKVOperations(t *testing.T) {
	cluster := newClusterWithLeader(t)
	rawKV := cluster.RawKV()
	ctx := context.Background()

	// RawPut
	err := rawKV.Put(ctx, []byte("cluster-key-1"), []byte("cluster-val-1"))
	require.NoError(t, err)

	// RawGet
	val, notFound, err := rawKV.Get(ctx, []byte("cluster-key-1"))
	require.NoError(t, err)
	assert.False(t, notFound)
	assert.Equal(t, []byte("cluster-val-1"), val)

	// RawDelete
	err = rawKV.Delete(ctx, []byte("cluster-key-1"))
	require.NoError(t, err)

	// Verify deleted
	_, notFound, err = rawKV.Get(ctx, []byte("cluster-key-1"))
	require.NoError(t, err)
	assert.True(t, notFound, "key should be deleted")

	t.Log("Cluster RawKV operations passed")
}

// TestClusterRawKVBatchPutAndScan tests batch RawPut and RawScan via Raft consensus.
func TestClusterRawKVBatchPutAndScan(t *testing.T) {
	cluster := newClusterWithLeader(t)
	rawKV := cluster.RawKV()
	ctx := context.Background()

	// Batch put 20 keys.
	for i := 0; i < 20; i++ {
		key := []byte(fmt.Sprintf("batch-cluster-%02d", i))
		val := []byte(fmt.Sprintf("val-%02d", i))
		err := rawKV.Put(ctx, key, val)
		require.NoError(t, err)
	}

	// Scan all keys.
	pairs, err := rawKV.Scan(ctx, []byte("batch-cluster-00"), []byte("batch-cluster-99"), 100)
	require.NoError(t, err)
	assert.Equal(t, 20, len(pairs), "scan should return all 20 keys")

	// Verify order.
	for i := 1; i < len(pairs); i++ {
		assert.LessOrEqual(t, string(pairs[i-1].Key), string(pairs[i].Key), "keys should be sorted")
	}

	t.Log("Cluster RawKV batch put and scan passed")
}
