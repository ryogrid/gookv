package e2e_external_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestRawBatchScan: Deferred — RawBatchScan has no CLI equivalent (BSCAN not implemented)
func TestRawBatchScan(t *testing.T) {

	node := e2elib.NewStandaloneNode(t)

	client := e2elib.DialTikvClient(t, node.Addr())
	ctx := context.Background()

	// Write keys in two separate ranges.
	// Range 1: "bs-a-00" .. "bs-a-04"
	// Range 2: "bs-b-00" .. "bs-b-02"
	for i := 0; i < 5; i++ {
		_, err := client.RawPut(ctx, &kvrpcpb.RawPutRequest{
			Key:   []byte(fmt.Sprintf("bs-a-%02d", i)),
			Value: []byte(fmt.Sprintf("val-a-%02d", i)),
		})
		require.NoError(t, err)
	}
	for i := 0; i < 3; i++ {
		_, err := client.RawPut(ctx, &kvrpcpb.RawPutRequest{
			Key:   []byte(fmt.Sprintf("bs-b-%02d", i)),
			Value: []byte(fmt.Sprintf("val-b-%02d", i)),
		})
		require.NoError(t, err)
	}

	// BatchScan both ranges.
	batchScanResp, err := client.RawBatchScan(ctx, &kvrpcpb.RawBatchScanRequest{
		Ranges: []*kvrpcpb.KeyRange{
			{StartKey: []byte("bs-a-00"), EndKey: []byte("bs-a-99")},
			{StartKey: []byte("bs-b-00"), EndKey: []byte("bs-b-99")},
		},
		EachLimit: 10,
	})
	require.NoError(t, err)
	// Expect 5 + 3 = 8 total pairs across both ranges.
	assert.Len(t, batchScanResp.GetKvs(), 8,
		"should get 8 KV pairs total across both ranges")

	// Verify that both range prefixes are represented.
	aCount, bCount := 0, 0
	for _, kv := range batchScanResp.GetKvs() {
		if len(kv.GetKey()) >= 4 && string(kv.GetKey()[:4]) == "bs-a" {
			aCount++
		}
		if len(kv.GetKey()) >= 4 && string(kv.GetKey()[:4]) == "bs-b" {
			bCount++
		}
	}
	assert.Equal(t, 5, aCount, "range A should have 5 keys")
	assert.Equal(t, 3, bCount, "range B should have 3 keys")

	// Test with EachLimit=2: should return at most 2 per range.
	batchScanResp2, err := client.RawBatchScan(ctx, &kvrpcpb.RawBatchScanRequest{
		Ranges: []*kvrpcpb.KeyRange{
			{StartKey: []byte("bs-a-00"), EndKey: []byte("bs-a-99")},
			{StartKey: []byte("bs-b-00"), EndKey: []byte("bs-b-99")},
		},
		EachLimit: 2,
	})
	require.NoError(t, err)
	assert.Len(t, batchScanResp2.GetKvs(), 4,
		"should get 2+2=4 KV pairs with EachLimit=2")

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
