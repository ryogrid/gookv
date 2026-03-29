package e2e_external_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestPDStoreRegistration verifies all stores are registered with PD.
func TestPDStoreRegistration(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// All 3 stores should be registered.
	out := e2elib.CLIExec(t, pdAddr, "STORE LIST")
	// Verify output contains each node's address.
	for _, node := range cluster.Nodes() {
		assert.True(t, strings.Contains(out, node.Addr()),
			"STORE LIST should contain node address %s", node.Addr())
	}

	// Verify each store has a valid address matching a node via STORE STATUS.
	nodeAddrs := make(map[string]bool)
	for _, node := range cluster.Nodes() {
		nodeAddrs[node.Addr()] = true
	}

	for i := 1; i <= 3; i++ {
		storeOut := e2elib.CLIExec(t, pdAddr, fmt.Sprintf("STORE STATUS %d", i))
		assert.True(t, strings.Contains(storeOut, "Address:"), "STORE STATUS should contain Address")
		// Verify the address matches one of the node addresses.
		foundAddr := false
		for addr := range nodeAddrs {
			if strings.Contains(storeOut, addr) {
				foundAddr = true
				break
			}
		}
		assert.True(t, foundAddr, "store %d address should match a node", i)
	}

	t.Log("PD store registration passed")
}

// TestPDRegionLeaderTracking verifies PD tracks the region leader.
func TestPDRegionLeaderTracking(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Wait for PD to know the leader.
	leaderStoreID := e2elib.CLIWaitForRegionLeader(t, pdAddr, "\"\"", 30*time.Second)
	assert.NotZero(t, leaderStoreID)

	// Verify the leader store exists via STORE STATUS.
	out := e2elib.CLIExec(t, pdAddr, fmt.Sprintf("STORE STATUS %d", leaderStoreID))
	assert.True(t, strings.Contains(out, fmt.Sprintf("Store ID:  %d", leaderStoreID)))
	assert.True(t, strings.Contains(out, "Address:"))

	t.Log("PD region leader tracking passed")
}

// TestPDLeaderFailover verifies PD updates to the new leader after the old one stops.
func TestPDLeaderFailover(t *testing.T) {
	cluster := newClusterWithLeader(t)
	pdAddr := cluster.PD().Addr()

	// Get initial leader via CLI.
	oldLeaderStoreID := e2elib.CLIWaitForRegionLeader(t, pdAddr, "", 30*time.Second)
	require.NotZero(t, oldLeaderStoreID)

	// Stop the leader node.
	leaderIdx := int(oldLeaderStoreID) - 1
	require.NoError(t, cluster.StopNode(leaderIdx))

	// Wait for PD to report a different leader via CLI.
	var newLeaderStoreID uint64
	e2elib.WaitForCondition(t, 30*time.Second, "new leader in PD", func() bool {
		out, _, err := e2elib.CLIExecRaw(t, pdAddr, "REGION \"\"")
		if err != nil {
			return false
		}
		id, ok := parseLeaderStoreIDFromOutput(out)
		if ok && id != 0 && id != oldLeaderStoreID {
			newLeaderStoreID = id
			return true
		}
		return false
	})

	// Verify the new leader's store address is valid via CLI.
	assert.NotEqual(t, oldLeaderStoreID, newLeaderStoreID, "leader should have changed")
	storeOut := e2elib.CLIExec(t, pdAddr, fmt.Sprintf("STORE STATUS %d", newLeaderStoreID))
	assert.True(t, strings.Contains(storeOut, "Address:"), "new leader store should have an address")
	assert.True(t, strings.Contains(storeOut, fmt.Sprintf("Store ID:  %d", newLeaderStoreID)))

	t.Log("PD leader failover passed")
}

// parseLeaderStoreIDFromOutput extracts leader store ID from REGION command output.
func parseLeaderStoreIDFromOutput(output string) (uint64, bool) {
	lines := strings.Split(output, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Leader:") {
			// Look for "store:<id>" pattern.
			idx := strings.Index(line, "store:")
			if idx < 0 {
				continue
			}
			rest := line[idx+len("store:"):]
			// Extract digits.
			var digits string
			for _, ch := range rest {
				if ch >= '0' && ch <= '9' {
					digits += string(ch)
				} else {
					break
				}
			}
			if digits != "" {
				id := uint64(0)
				for _, ch := range digits {
					id = id*10 + uint64(ch-'0')
				}
				return id, true
			}
		}
	}
	return 0, false
}
