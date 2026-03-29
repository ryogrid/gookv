package e2e_external_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestRawBatchScan tests the BSCAN command via CLI (--addr mode).
func TestRawBatchScan(t *testing.T) {
	node := e2elib.NewStandaloneNode(t)
	addr := node.Addr()

	// Write keys in two separate ranges.
	for i := 0; i < 5; i++ {
		e2elib.CLINodeExec(t, addr, fmt.Sprintf("PUT bs-a-%02d val-a-%02d", i, i))
	}
	for i := 0; i < 3; i++ {
		e2elib.CLINodeExec(t, addr, fmt.Sprintf("PUT bs-b-%02d val-b-%02d", i, i))
	}

	// BatchScan both ranges with EACH_LIMIT 10.
	out := e2elib.CLINodeExec(t, addr, "BSCAN bs-a-00 bs-a-99 bs-b-00 bs-b-99 EACH_LIMIT 10")
	// Expect 5 + 3 = 8 total pairs across both ranges.
	aCount, bCount := 0, 0
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "bs-a-") {
			aCount++
		}
		if strings.Contains(line, "bs-b-") {
			bCount++
		}
	}
	assert.Equal(t, 5, aCount, "range A should have 5 keys, output:\n%s", out)
	assert.Equal(t, 3, bCount, "range B should have 3 keys, output:\n%s", out)

	// Test with EACH_LIMIT 2: should return at most 2 per range.
	out2 := e2elib.CLINodeExec(t, addr, "BSCAN bs-a-00 bs-a-99 bs-b-00 bs-b-99 EACH_LIMIT 2")
	total := 0
	for _, line := range strings.Split(out2, "\n") {
		if strings.Contains(line, "bs-a-") || strings.Contains(line, "bs-b-") {
			total++
		}
	}
	assert.Equal(t, 4, total, "should get 2+2=4 KV pairs with EACH_LIMIT 2, output:\n%s", out2)

	t.Log("RawBatchScan passed")
}

// TestRawGetKeyTTL tests the TTL command via CLI.
// Verifies TTL output for keys without TTL and not-found for missing keys.
func TestRawGetKeyTTL(t *testing.T) {
	node := e2elib.NewStandaloneNode(t)
	addr := node.Addr()

	// Put a key without TTL.
	e2elib.CLINodeExec(t, addr, "PUT ttl-test-key ttl-test-value")

	// TTL: key exists, no TTL set.
	ttlOut := e2elib.CLINodeExec(t, addr, "TTL ttl-test-key")
	assert.True(t, strings.Contains(ttlOut, "TTL:"),
		"TTL output should contain 'TTL:', got: %s", ttlOut)

	// TTL for non-existent key — CLI returns error (exit 1), so use CLIExecRaw pattern.
	_, stderr, err := e2elib.CLINodeExecRaw(addr, "TTL nonexistent-key")
	assert.Error(t, err, "TTL on nonexistent key should return error")
	assert.True(t, strings.Contains(stderr, "not found") || strings.Contains(stderr, "key not found"),
		"nonexistent key should report not found, got stderr: %s", stderr)

	t.Log("RawGetKeyTTL passed")
}

// TestRawCompareAndSwap tests the CAS command via CLI.
// Covers: successful swap, failed swap (wrong previous value),
// create (NOT_EXIST), and verify via GET.
func TestRawCompareAndSwap(t *testing.T) {
	node := e2elib.NewStandaloneNode(t)
	addr := node.Addr()

	key := "cas-key"

	// Subtest 1: Create with NOT_EXIST.
	t.Run("CreateWhenNotExist", func(t *testing.T) {
		casOut := e2elib.CLINodeExec(t, addr, fmt.Sprintf("CAS %s initial _ NOT_EXIST", key))
		assert.True(t, strings.Contains(casOut, "OK (swapped)"),
			"CAS create should succeed when key doesn't exist, got: %s", casOut)

		// Verify the key was written.
		val, found := e2elib.CLINodeGet(t, addr, key)
		require.True(t, found)
		assert.Equal(t, "initial", val)
	})

	// Subtest 2: Successful CAS with correct previous value.
	t.Run("SwapWithCorrectPreviousValue", func(t *testing.T) {
		casOut := e2elib.CLINodeExec(t, addr, fmt.Sprintf("CAS %s updated initial", key))
		assert.True(t, strings.Contains(casOut, "OK (swapped)"),
			"CAS should succeed with matching previous value, got: %s", casOut)

		// Verify the key was updated.
		val, found := e2elib.CLINodeGet(t, addr, key)
		require.True(t, found)
		assert.Equal(t, "updated", val)
	})

	// Subtest 3: Failed CAS with wrong previous value.
	t.Run("FailWithWrongPreviousValue", func(t *testing.T) {
		casOut := e2elib.CLINodeExec(t, addr, fmt.Sprintf("CAS %s should-not-write wrong-value", key))
		assert.True(t, strings.Contains(casOut, "FAILED (not swapped)"),
			"CAS should fail with wrong previous value, got: %s", casOut)

		// Verify the key was NOT modified.
		val, found := e2elib.CLINodeGet(t, addr, key)
		require.True(t, found)
		assert.Equal(t, "updated", val)
	})

	t.Log("RawCompareAndSwap passed")
}

// TestRawChecksum tests the CHECKSUM command via CLI.
// It puts several keys, calls CHECKSUM on the range, and verifies
// that the output contains TotalKvs information.
func TestRawChecksum(t *testing.T) {
	node := e2elib.NewStandaloneNode(t)
	addr := node.Addr()

	// Write 5 keys in a known range.
	const keyCount = 5
	for i := 0; i < keyCount; i++ {
		e2elib.CLINodeExec(t, addr, fmt.Sprintf("PUT csum-%02d csum-val-%02d", i, i))
	}

	// Call CHECKSUM on the range.
	csumOut := e2elib.CLINodeExec(t, addr, "CHECKSUM csum-00 csum-99")
	assert.True(t, strings.Contains(csumOut, "Total keys:"),
		"CHECKSUM output should contain 'TotalKvs:', got: %s", csumOut)

	// Call CHECKSUM again and verify the data lines (excluding timing) are stable.
	csumOut2 := e2elib.CLINodeExec(t, addr, "CHECKSUM csum-00 csum-99")
	// Strip timing lines (e.g., "(3.0ms)") before comparison.
	stripTiming := func(s string) string {
		var lines []string
		for _, line := range strings.Split(s, "\n") {
			trimmed := strings.TrimSpace(line)
			if trimmed != "" && !strings.HasPrefix(trimmed, "(") {
				lines = append(lines, trimmed)
			}
		}
		return strings.Join(lines, "\n")
	}
	assert.Equal(t, stripTiming(csumOut), stripTiming(csumOut2),
		"CHECKSUM output should be deterministic for unchanged data")

	t.Log("RawChecksum passed")
}
