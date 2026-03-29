package e2e_external_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestClusterRawKVOperations tests RawPut, RawGet, RawDelete via Raft consensus.
func TestClusterRawKVOperations(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// RawPut
	e2elib.CLIPut(t, pdAddr, "cluster-key-1", "cluster-val-1")

	// RawGet
	val, found := e2elib.CLIGet(t, pdAddr, "cluster-key-1")
	assert.True(t, found)
	assert.Equal(t, "cluster-val-1", val)

	// RawDelete
	e2elib.CLIDelete(t, pdAddr, "cluster-key-1")

	// Verify deleted
	_, found = e2elib.CLIGet(t, pdAddr, "cluster-key-1")
	assert.False(t, found, "key should be deleted")

	t.Log("Cluster RawKV operations passed")
}

// TestClusterRawKVBatchPutAndScan tests batch RawPut and RawScan via Raft consensus.
func TestClusterRawKVBatchPutAndScan(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Batch put 20 keys.
	for i := 0; i < 20; i++ {
		key := fmt.Sprintf("batch-cluster-%02d", i)
		val := fmt.Sprintf("val-%02d", i)
		e2elib.CLIPut(t, pdAddr, key, val)
	}

	// Scan all keys.
	out := e2elib.CLIExec(t, pdAddr, "SCAN batch-cluster-00 batch-cluster-99 LIMIT 100")
	// Count lines that contain "batch-cluster-" to determine number of scanned keys.
	lines := strings.Split(out, "\n")
	count := 0
	for _, line := range lines {
		if strings.Contains(line, "batch-cluster-") {
			count++
		}
	}
	assert.Equal(t, 20, count, "scan should return all 20 keys")

	t.Log("Cluster RawKV batch put and scan passed")
}
