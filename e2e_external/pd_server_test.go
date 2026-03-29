package e2e_external_test

import (
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// startPDOnly creates a standalone PD node (no gookv-server needed).
func startPDOnly(t *testing.T) *e2elib.PDNode {
	t.Helper()
	e2elib.SkipIfNoBinary(t, "gookv-pd")

	alloc := e2elib.NewPortAllocator()
	t.Cleanup(func() { alloc.ReleaseAll() })

	pd := e2elib.NewPDNode(t, alloc, e2elib.PDNodeConfig{})
	require.NoError(t, pd.Start())
	require.NoError(t, pd.WaitForReady(30*1e9)) // 30 seconds
	return pd
}

// TestPDServerBootstrapAndTSO verifies PD bootstrap, TSO allocation, and cluster metadata.
func TestPDServerBootstrapAndTSO(t *testing.T) {
	pd := startPDOnly(t)
	pdAddr := pd.Addr()

	// Before bootstrap, IsBootstrapped should be false.
	out := e2elib.CLIExec(t, pdAddr, "IS BOOTSTRAPPED")
	assert.Equal(t, "false", parseScalarValue(out), "cluster should not be bootstrapped initially")

	// Bootstrap the cluster.
	e2elib.CLIExec(t, pdAddr, "BOOTSTRAP 1 127.0.0.1:20160")

	// After bootstrap, IsBootstrapped should be true.
	out = e2elib.CLIExec(t, pdAddr, "IS BOOTSTRAPPED")
	assert.Equal(t, "true", parseScalarValue(out), "cluster should be bootstrapped")

	// TSO allocation: timestamps must be monotonically increasing.
	out1 := e2elib.CLIExec(t, pdAddr, "TSO")
	ts1 := parseTSOTimestamp(t, out1)
	out2 := e2elib.CLIExec(t, pdAddr, "TSO")
	ts2 := parseTSOTimestamp(t, out2)
	assert.Greater(t, ts2, ts1, "TSO must be monotonically increasing")

	// AllocID: IDs must be unique and increasing.
	out1 = e2elib.CLIExec(t, pdAddr, "ALLOC ID")
	id1 := parseUint64Value(t, out1)
	out2 = e2elib.CLIExec(t, pdAddr, "ALLOC ID")
	id2 := parseUint64Value(t, out2)
	assert.Greater(t, id2, id1, "AllocID must be monotonically increasing")

	t.Log("PD bootstrap, TSO, and AllocID passed")
}

// TestPDServerStoreAndRegionMetadata verifies store and region metadata management via PD.
func TestPDServerStoreAndRegionMetadata(t *testing.T) {
	pd := startPDOnly(t)
	pdAddr := pd.Addr()

	// Bootstrap.
	e2elib.CLIExec(t, pdAddr, "BOOTSTRAP 1 127.0.0.1:20160")

	// Put additional stores.
	e2elib.CLIExec(t, pdAddr, "PUT STORE 2 127.0.0.1:20161")
	e2elib.CLIExec(t, pdAddr, "PUT STORE 3 127.0.0.1:20162")

	// GetStore should return the stored metadata.
	out := e2elib.CLIExec(t, pdAddr, "STORE STATUS 2")
	assert.True(t, strings.Contains(out, "Store ID:  2"))
	assert.True(t, strings.Contains(out, "127.0.0.1:20161"))

	// GetRegionByID should return the bootstrapped region.
	out = e2elib.CLIExec(t, pdAddr, "REGION ID 1")
	assert.True(t, strings.Contains(out, "Region ID:  1"))

	// GetRegion by key should return region covering the key.
	out = e2elib.CLIExec(t, pdAddr, "REGION some-key")
	assert.True(t, strings.Contains(out, "Region ID:"),
		"region should cover any key since it has no start/end key bounds")

	t.Log("PD store and region metadata passed")
}

// TestPDAskBatchSplitAndReport verifies the AskBatchSplit and ReportBatchSplit RPCs.
func TestPDAskBatchSplitAndReport(t *testing.T) {
	pd := startPDOnly(t)
	pdAddr := pd.Addr()

	// Bootstrap.
	e2elib.CLIExec(t, pdAddr, "BOOTSTRAP 1 127.0.0.1:20160")

	// AskBatchSplit for 1 split.
	out := e2elib.CLIExec(t, pdAddr, "ASK SPLIT 1 1")
	// Output is a table with NewRegionID and NewPeerIDs columns.
	// Verify we got a non-zero new region ID.
	assert.True(t, strings.Contains(out, "NewRegionID"), "should have NewRegionID column")
	// Parse the table to get the new region ID and peer IDs.
	newRegionID, newPeerIDs := parseAskSplitRow(t, out, 0)
	assert.NotZero(t, newRegionID, "new region ID should be non-zero")
	assert.NotEmpty(t, newPeerIDs, "new peer IDs should be allocated")

	// Report the split: REPORT SPLIT <leftRegionID> <rightRegionID> <splitKey>
	e2elib.CLIExec(t, pdAddr, "REPORT SPLIT 1 "+strconv.FormatUint(newRegionID, 10)+" m")

	// Verify PD now has both regions.
	outLeft := e2elib.CLIExec(t, pdAddr, "REGION abc")
	assert.True(t, strings.Contains(outLeft, "Region ID:  1"),
		"key 'abc' should map to the left region (region 1)")

	outRight := e2elib.CLIExec(t, pdAddr, "REGION xyz")
	assert.True(t, strings.Contains(outRight, "Region ID:  "+strconv.FormatUint(newRegionID, 10)),
		"key 'xyz' should map to the right region")

	t.Log("PD AskBatchSplit and ReportBatchSplit passed")
}

// TestPDStoreHeartbeat verifies sending store heartbeats to PD.
func TestPDStoreHeartbeat(t *testing.T) {
	pd := startPDOnly(t)
	pdAddr := pd.Addr()

	// Bootstrap.
	e2elib.CLIExec(t, pdAddr, "BOOTSTRAP 1 127.0.0.1:20160")

	// Send a store heartbeat (should not error).
	e2elib.CLIExec(t, pdAddr, "STORE HEARTBEAT 1 REGIONS 5")

	t.Log("PD store heartbeat passed")
}

// parseScalarValue extracts a scalar value from CLI output (first line, stripped of timing).
func parseScalarValue(output string) string {
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 {
		return ""
	}
	// Return the first line, which is the scalar value.
	// The timing line starts with '(' and is on a separate line.
	return strings.TrimSpace(lines[0])
}

// parseUint64Value extracts a uint64 from the scalar CLI output.
func parseUint64Value(t *testing.T, output string) uint64 {
	t.Helper()
	s := parseScalarValue(output)
	v, err := strconv.ParseUint(s, 10, 64)
	require.NoError(t, err, "failed to parse uint64 from: %q", s)
	return v
}

// parseAskSplitRow extracts the NewRegionID and NewPeerIDs from ASK SPLIT table output.
// rowIdx selects which row (0-based) to parse.
func parseAskSplitRow(t *testing.T, output string, rowIdx int) (uint64, []uint64) {
	t.Helper()
	lines := strings.Split(output, "\n")
	dataRows := 0
	headerSeen := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip border lines
		if strings.Trim(line, "+-") == "" {
			continue
		}
		if !strings.HasPrefix(line, "|") {
			continue
		}
		// First data line with | is the header
		if !headerSeen {
			headerSeen = true
			continue
		}
		if dataRows == rowIdx {
			// Parse this row
			cells := strings.Split(line, "|")
			// cells[0] is empty (before first |), cells[1] is NewRegionID, cells[2] is NewPeerIDs
			if len(cells) < 3 {
				t.Fatalf("unexpected ASK SPLIT row format: %s", line)
			}
			regionIDStr := strings.TrimSpace(cells[1])
			peerIDsStr := strings.TrimSpace(cells[2])

			regionID, err := strconv.ParseUint(regionIDStr, 10, 64)
			require.NoError(t, err, "failed to parse NewRegionID from: %q", regionIDStr)

			var peerIDs []uint64
			for _, pidStr := range strings.Split(peerIDsStr, ",") {
				pidStr = strings.TrimSpace(pidStr)
				if pidStr == "" {
					continue
				}
				pid, err := strconv.ParseUint(pidStr, 10, 64)
				require.NoError(t, err, "failed to parse peer ID from: %q", pidStr)
				peerIDs = append(peerIDs, pid)
			}
			return regionID, peerIDs
		}
		dataRows++
	}
	t.Fatalf("ASK SPLIT row %d not found in output: %s", rowIdx, output)
	return 0, nil
}
