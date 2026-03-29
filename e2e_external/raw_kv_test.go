package e2e_external_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestRawKVPutGetDelete tests basic raw KV operations via CLI.
func TestRawKVPutGetDelete(t *testing.T) {
	node := e2elib.NewStandaloneNode(t)
	addr := node.Addr()

	// PUT
	e2elib.CLINodeExec(t, addr, "PUT raw-key-1 raw-value-1")

	// GET
	val, found := e2elib.CLINodeGet(t, addr, "raw-key-1")
	require.True(t, found, "key should be found after PUT")
	assert.Equal(t, "raw-value-1", val)

	// DELETE
	e2elib.CLINodeExec(t, addr, "DELETE raw-key-1")

	// GET after delete
	_, found = e2elib.CLINodeGet(t, addr, "raw-key-1")
	assert.False(t, found, "key should be deleted")

	t.Log("Raw KV put/get/delete passed")
}

// TestRawKVBatchOperations tests batch raw KV operations via CLI.
func TestRawKVBatchOperations(t *testing.T) {
	node := e2elib.NewStandaloneNode(t)
	addr := node.Addr()

	// BPUT 10 keys
	bputArgs := ""
	for i := 0; i < 10; i++ {
		bputArgs += fmt.Sprintf(" batch-key-%02d batch-value-%02d", i, i)
	}
	e2elib.CLINodeExec(t, addr, "BPUT"+bputArgs)

	// BGET 5 even-numbered keys
	bgetArgs := ""
	for i := 0; i < 5; i++ {
		bgetArgs += fmt.Sprintf(" batch-key-%02d", i*2)
	}
	bgetOut := e2elib.CLINodeExec(t, addr, "BGET"+bgetArgs)
	// Verify all 5 even keys appear in output
	for i := 0; i < 5; i++ {
		key := fmt.Sprintf("batch-key-%02d", i*2)
		assert.True(t, strings.Contains(bgetOut, key),
			"BGET output should contain %s", key)
	}

	// SCAN all 10 keys
	scanOut := e2elib.CLINodeExec(t, addr, "SCAN batch-key-00 batch-key-99 LIMIT 100")
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("batch-key-%02d", i)
		assert.True(t, strings.Contains(scanOut, key),
			"SCAN output should contain %s", key)
	}

	// SCAN with limit 3
	scanOut3 := e2elib.CLINodeExec(t, addr, "SCAN batch-key-00 batch-key-99 LIMIT 3")
	// Count how many batch-key entries appear
	matchCount := 0
	for i := 0; i < 10; i++ {
		key := fmt.Sprintf("batch-key-%02d", i)
		if strings.Contains(scanOut3, key) {
			matchCount++
		}
	}
	assert.Equal(t, 3, matchCount, "scan with limit 3 should return 3 keys")

	t.Log("Raw KV batch operations passed")
}

// TestRawKVDeleteRange tests the DELETE RANGE operation via CLI.
func TestRawKVDeleteRange(t *testing.T) {
	node := e2elib.NewStandaloneNode(t)
	addr := node.Addr()

	// Write some keys.
	for i := 0; i < 5; i++ {
		e2elib.CLINodeExec(t, addr, fmt.Sprintf("PUT dr-key-%02d dr-val-%02d", i, i))
	}

	// DELETE RANGE: delete keys 01-03.
	e2elib.CLINodeExec(t, addr, "DELETE RANGE dr-key-01 dr-key-04")

	// Verify: key-00 and key-04 still exist.
	val00, found00 := e2elib.CLINodeGet(t, addr, "dr-key-00")
	assert.True(t, found00, "key-00 should still exist")
	assert.Equal(t, "dr-val-00", val00)

	val04, found04 := e2elib.CLINodeGet(t, addr, "dr-key-04")
	assert.True(t, found04, "key-04 should still exist")
	assert.Equal(t, "dr-val-04", val04)

	// Verify: keys 01, 02, 03 deleted.
	for i := 1; i <= 3; i++ {
		_, found := e2elib.CLINodeGet(t, addr, fmt.Sprintf("dr-key-%02d", i))
		assert.False(t, found, "key-%02d should be deleted", i)
	}

	t.Log("Raw KV delete range passed")
}
