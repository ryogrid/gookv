package e2e_external_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestRegionSplitWithPD tests the end-to-end region split flow via PD CLI commands.
// This test exercises the PD split ID allocation and reporting path.
func TestRegionSplitWithPD(t *testing.T) {
	e2elib.SkipIfNoBinary(t, "gookv-pd")

	alloc := e2elib.NewPortAllocator()
	t.Cleanup(func() { alloc.ReleaseAll() })

	pd := e2elib.NewPDNode(t, alloc, e2elib.PDNodeConfig{})
	require.NoError(t, pd.Start())
	require.NoError(t, pd.WaitForReady(15*time.Second))

	pdAddr := pd.Addr()

	// Bootstrap cluster.
	e2elib.CLIExec(t, pdAddr, "BOOTSTRAP 1 127.0.0.1:20160")

	// Request split IDs from PD.
	out := e2elib.CLIExec(t, pdAddr, "ASK SPLIT 1 1")
	assert.Contains(t, out, "NewRegionID", "should get split ID table")

	newRegionID, peerIDs := parseAskSplitRow(t, out, 0)
	assert.NotZero(t, newRegionID, "new region ID should be non-zero")
	require.NotEmpty(t, peerIDs, "should have new peer IDs")

	// Report the split: REPORT SPLIT <leftRegionID> <rightRegionID> <splitKey>
	e2elib.CLIExec(t, pdAddr, "REPORT SPLIT 1 "+strconv.FormatUint(newRegionID, 10)+" m")

	// Verify PD metadata: key before split point should be in left region.
	e2elib.CLIWaitForCondition(t, pdAddr, "REGION a", func(output string) bool {
		// The left region should have EndKey containing "m"
		return strings.Contains(output, "Region ID:  1") && strings.Contains(output, "6d") // "m" in hex
	}, 10*time.Second)

	outLeft := e2elib.CLIExec(t, pdAddr, "REGION a")
	assert.Contains(t, outLeft, "Region ID:  1", "left region should keep original ID")
	// EndKey should contain the hex encoding of "m" (0x6d)
	assert.Contains(t, outLeft, "6d", "left region EndKey should be 'm'")

	// Key after split point should be in right region.
	outRight := e2elib.CLIExec(t, pdAddr, "REGION z")
	assert.Contains(t, outRight, "Region ID:  "+strconv.FormatUint(newRegionID, 10),
		"right region should have new ID")
	// StartKey should contain the hex encoding of "m" (0x6d)
	assert.Contains(t, outRight, "6d", "right region StartKey should be 'm'")

	t.Log("Region split with PD passed")
}
