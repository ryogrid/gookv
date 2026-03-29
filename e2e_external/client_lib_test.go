package e2e_external_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// --- Region Cache Tests ---

func TestClientRegionCacheMiss(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// Cold cache: Put and Get should succeed by querying PD.
	e2elib.CLIPut(t, pdAddr, "key1", "val1")

	val, found := e2elib.CLIGet(t, pdAddr, "key1")
	assert.True(t, found)
	assert.Equal(t, "val1", val)
}

func TestClientRegionCacheHit(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// First put populates cache.
	e2elib.CLIPut(t, pdAddr, "a", "1")

	// Second put should use cached region.
	e2elib.CLIPut(t, pdAddr, "b", "2")

	val, found := e2elib.CLIGet(t, pdAddr, "a")
	assert.True(t, found)
	assert.Equal(t, "1", val)

	val, found = e2elib.CLIGet(t, pdAddr, "b")
	assert.True(t, found)
	assert.Equal(t, "2", val)
}

// --- Store Resolution Tests ---

func TestClientStoreResolution(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// Client discovers store address from PD, connects, and writes.
	e2elib.CLIPut(t, pdAddr, "key", "val")

	val, found := e2elib.CLIGet(t, pdAddr, "key")
	assert.True(t, found)
	assert.Equal(t, "val", val)
}

// --- Batch Operation Tests ---

func TestClientBatchGetAcrossRegions(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// Put keys in different regions.
	e2elib.CLIPut(t, pdAddr, "apple", "1")
	e2elib.CLIPut(t, pdAddr, "mango", "2")

	// BatchGet across regions.
	out := e2elib.CLIExec(t, pdAddr, "BGET apple mango")
	assert.True(t, strings.Contains(out, "apple"), "output should contain key 'apple'")
	assert.True(t, strings.Contains(out, "1"), "output should contain value '1'")
	assert.True(t, strings.Contains(out, "mango"), "output should contain key 'mango'")
	assert.True(t, strings.Contains(out, "2"), "output should contain value '2'")
}

func TestClientBatchPutAcrossRegions(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// BatchPut keys across regions.
	e2elib.CLIExec(t, pdAddr, "BPUT a 1 z 2")

	val, found := e2elib.CLIGet(t, pdAddr, "a")
	assert.True(t, found)
	assert.Equal(t, "1", val)

	val, found = e2elib.CLIGet(t, pdAddr, "z")
	assert.True(t, found)
	assert.Equal(t, "2", val)
}

// --- Scan Tests ---

func TestClientScanAcrossRegions(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// Put keys in both regions.
	keys := []string{"a", "b", "c", "m", "n"}
	for i, k := range keys {
		e2elib.CLIPut(t, pdAddr, k, string(rune('0'+i)))
	}

	// Scan all.
	out := e2elib.CLIExec(t, pdAddr, "SCAN \"\" \"\" LIMIT 100")
	// Verify all keys appear in the output.
	for _, k := range keys {
		assert.True(t, strings.Contains(out, k), "scan output should contain key %q", k)
	}
}

func TestClientScanWithLimit(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	keys := []string{"a", "b", "c", "m", "n"}
	for i, k := range keys {
		e2elib.CLIPut(t, pdAddr, k, string(rune('0'+i)))
	}

	// Scan with limit 3 -- should only return 3 keys from the first region.
	out := e2elib.CLIExec(t, pdAddr, "SCAN \"\" \"\" LIMIT 3")
	// Count the number of key lines in the output (each data row has a key).
	// We expect exactly 3 results.
	lines := strings.Split(strings.TrimSpace(out), "\n")
	dataLines := 0
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip border lines (only + and -)
		if strings.Trim(line, "+-") == "" {
			continue
		}
		if strings.HasPrefix(line, "|") {
			dataLines++
		}
	}
	// Subtract 1 for the header row.
	if dataLines > 0 {
		dataLines--
	}
	require.Equal(t, 3, dataLines, fmt.Sprintf("expected 3 data rows, got output:\n%s", out))
}

// --- Advanced Operation Tests ---

func TestClientCompareAndSwap(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// Put a key in region 2.
	e2elib.CLIPut(t, pdAddr, "zebra", "old")

	// CAS: replace "old" with "new".
	out := e2elib.CLIExec(t, pdAddr, "CAS zebra new old")
	assert.True(t, strings.Contains(out, "OK (swapped)"), "CAS should succeed, got: %s", out)

	// Verify.
	val, found := e2elib.CLIGet(t, pdAddr, "zebra")
	assert.True(t, found)
	assert.Equal(t, "new", val)
}

// --- Delete Tests ---

func TestClientBatchDeleteAcrossRegions(t *testing.T) {
	cluster, _ := newClientCluster(t)
	pdAddr := cluster.PD().Addr()

	// Put keys.
	e2elib.CLIPut(t, pdAddr, "alpha", "v1")
	e2elib.CLIPut(t, pdAddr, "zeta", "v2")

	// Delete across regions.
	e2elib.CLIExec(t, pdAddr, "BDELETE alpha zeta")

	// Verify deleted.
	_, found := e2elib.CLIGet(t, pdAddr, "alpha")
	assert.False(t, found)

	_, found = e2elib.CLIGet(t, pdAddr, "zeta")
	assert.False(t, found)
}
