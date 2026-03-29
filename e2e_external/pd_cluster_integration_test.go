package e2e_external_test

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestPDClusterStoreAndRegionHeartbeat verifies the heartbeat loop between
// gookv-server nodes and PD. The servers automatically send heartbeats.
func TestPDClusterStoreAndRegionHeartbeat(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Wait for all stores to be registered via heartbeats.
	storeCount := e2elib.CLIWaitForStoreCount(t, pdAddr, 3, 30*time.Second)
	assert.Equal(t, 3, storeCount)

	// Verify each store has a correct address via STORE STATUS.
	for i := 1; i <= 3; i++ {
		out := e2elib.CLIExec(t, pdAddr, fmt.Sprintf("STORE STATUS %d", i))
		assert.Contains(t, out, fmt.Sprintf("Store ID:  %d", i))
		assert.Contains(t, out, "Address:")
	}

	// Verify region heartbeat: PD should know about region 1.
	out := e2elib.CLIExec(t, pdAddr, "REGION \"\"")
	assert.Contains(t, out, "Region ID:  1", "region 1 should be registered")
	// Leader should be set after heartbeats.
	assert.NotContains(t, out, "Leader:    (none)", "region should have a leader after heartbeats")

	t.Log("PD cluster store and region heartbeat passed")
}

// TestPDClusterTSOForTransactions tests using PD TSO for transaction timestamps.
func TestPDClusterTSOForTransactions(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Allocate 100 timestamps and verify monotonicity.
	var prevTS uint64
	for i := 0; i < 100; i++ {
		out := e2elib.CLIExec(t, pdAddr, "TSO")
		currentTS := parseTSOTimestamp(t, out)
		assert.Greater(t, currentTS, prevTS, "TSO should be strictly increasing (iter %d)", i)
		prevTS = currentTS
	}

	t.Log("PD cluster TSO for transactions passed")
}

// TestPDClusterGCSafePoint tests GC safe point management via PD.
func TestPDClusterGCSafePoint(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Initial GC safe point should be 0.
	out := e2elib.CLIExec(t, pdAddr, "GC SAFEPOINT")
	assert.Contains(t, out, "0", "initial GC safe point should be 0")

	// Update GC safe point.
	out = e2elib.CLIExec(t, pdAddr, "GC SAFEPOINT SET 1000")
	assert.Contains(t, out, "1000")

	// Verify it was updated.
	out = e2elib.CLIExec(t, pdAddr, "GC SAFEPOINT")
	assert.Contains(t, out, "1000")

	// GC safe point should not go backwards.
	e2elib.CLIExec(t, pdAddr, "GC SAFEPOINT SET 500")
	// PD should keep the higher value.
	out = e2elib.CLIExec(t, pdAddr, "GC SAFEPOINT")
	assert.Contains(t, out, "1000", "GC safe point should not go backwards")

	t.Log("PD cluster GC safe point passed")
}

// parseTSOTimestamp extracts the raw timestamp value from TSO command output.
// The output format is: "Timestamp:  <N>\n  physical: <P> (<date>)\n  logical:  <L>"
func parseTSOTimestamp(t *testing.T, output string) uint64 {
	t.Helper()
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "Timestamp:") {
			parts := strings.Fields(line)
			if len(parts) >= 2 {
				ts, err := strconv.ParseUint(parts[1], 10, 64)
				require.NoError(t, err, "failed to parse TSO timestamp from: %s", line)
				return ts
			}
		}
	}
	t.Fatalf("failed to find Timestamp in TSO output: %s", output)
	return 0
}
