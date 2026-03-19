package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"github.com/ryogrid/gookv/internal/engine/rocks"
	"github.com/ryogrid/gookv/internal/storage/mvcc"
	"github.com/ryogrid/gookv/pkg/cfnames"
	"github.com/ryogrid/gookv/pkg/keys"
	"github.com/ryogrid/gookv/pkg/txntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseCommand(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"scan --db /tmp", "scan"},
		{"get --key abc", "get"},
		{"mvcc --key abc", "mvcc"},
		{"dump --db /tmp", "dump"},
		{"size --db /tmp", "size"},
		{"compact --db /tmp", "compact"},
		{"help", "help"},
		{"", ""},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			assert.Equal(t, tt.expected, ParseCommand(tt.input))
		})
	}
}

func TestTryPrintable(t *testing.T) {
	tests := []struct {
		input    []byte
		expected string
	}{
		{[]byte("hello"), "hello"},
		{[]byte{0x00, 0x01, 0x02}, "000102"},
		{[]byte("abc\x00def"), "61626300646566"},
		{[]byte(""), ""},
	}

	for _, tt := range tests {
		assert.Equal(t, tt.expected, tryPrintable(tt.input))
	}
}

func TestFormatSize(t *testing.T) {
	tests := []struct {
		input    int64
		expected string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.00 KB"},
		{1024 * 1024, "1.00 MB"},
		{1024 * 1024 * 1024, "1.00 GB"},
		{1536 * 1024, "1.50 MB"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, formatSize(tt.input))
		})
	}
}

func TestWriteTypeStr(t *testing.T) {
	assert.Equal(t, "Put", writeTypeStr('P'))
	assert.Equal(t, "Delete", writeTypeStr('D'))
	assert.Equal(t, "Rollback", writeTypeStr('R'))
	assert.Equal(t, "Lock", writeTypeStr('L'))
}

func TestUsageNotEmpty(t *testing.T) {
	assert.NotEmpty(t, usage)
	assert.Contains(t, usage, "gookv-ctl")
	assert.Contains(t, usage, "scan")
	assert.Contains(t, usage, "get")
	assert.Contains(t, usage, "mvcc")
	assert.Contains(t, usage, "compact")
	assert.Contains(t, usage, "region")
}

// captureOutput captures stdout during fn execution.
func captureOutput(fn func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	fn()

	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	io.Copy(&buf, r)
	return buf.String()
}

// testDB wraps an engine and its path. The engine must be closed before
// calling CLI commands (which open the DB themselves).
type testDB struct {
	eng  *rocks.Engine
	path string
}

func newTestDB(t *testing.T) *testDB {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test-db")
	eng, err := rocks.Open(path)
	require.NoError(t, err)
	return &testDB{eng: eng, path: path}
}

// closeAndPath closes the engine and returns the path for CLI commands.
func (db *testDB) closeAndPath(t *testing.T) string {
	t.Helper()
	require.NoError(t, db.eng.Close())
	return db.path
}

// ============================================================================
// Region command tests
// ============================================================================

func TestIsRegionStateKey(t *testing.T) {
	key := keys.RegionStateKey(1)
	assert.True(t, isRegionStateKey(key))
	assert.False(t, isRegionStateKey([]byte("short")))
	assert.False(t, isRegionStateKey(keys.RaftStateKey(1)))
}

func TestCmdRegionSingle(t *testing.T) {
	db := newTestDB(t)

	state := &raft_serverpb.RegionLocalState{
		State: raft_serverpb.PeerState_Normal,
		Region: &metapb.Region{
			Id:       42,
			StartKey: []byte{0x01},
			EndKey:   []byte{0x02},
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: 1,
				Version: 1,
			},
		},
	}
	data, err := state.Marshal()
	require.NoError(t, err)
	require.NoError(t, db.eng.Put(cfnames.CFRaft, keys.RegionStateKey(42), data))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdRegion([]string{"--db", dbPath, "--id", "42"})
	})

	assert.Contains(t, output, "Region ID: 42")
	assert.Contains(t, output, "Normal")
}

func TestCmdRegionAll(t *testing.T) {
	db := newTestDB(t)

	for _, id := range []uint64{1, 2, 3} {
		state := &raft_serverpb.RegionLocalState{
			State: raft_serverpb.PeerState_Normal,
			Region: &metapb.Region{
				Id:          id,
				RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
			},
		}
		data, err := state.Marshal()
		require.NoError(t, err)
		require.NoError(t, db.eng.Put(cfnames.CFRaft, keys.RegionStateKey(id), data))
	}
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdRegion([]string{"--db", dbPath, "--all"})
	})

	assert.Contains(t, output, "Total: 3 regions")
}

func TestCmdRegionNotFound(t *testing.T) {
	db := newTestDB(t)
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdRegion([]string{"--db", dbPath, "--id", "999"})
	})

	assert.Contains(t, output, "Region 999 not found")
}

func TestCmdRegionLimit(t *testing.T) {
	db := newTestDB(t)

	for i := uint64(1); i <= 10; i++ {
		state := &raft_serverpb.RegionLocalState{
			State: raft_serverpb.PeerState_Normal,
			Region: &metapb.Region{
				Id:          i,
				RegionEpoch: &metapb.RegionEpoch{ConfVer: 1, Version: 1},
			},
		}
		data, err := state.Marshal()
		require.NoError(t, err)
		require.NoError(t, db.eng.Put(cfnames.CFRaft, keys.RegionStateKey(i), data))
	}
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdRegion([]string{"--db", dbPath, "--limit", "3"})
	})

	assert.Contains(t, output, "Total: 3 regions")
}

func TestCmdRegionEmptyDB(t *testing.T) {
	db := newTestDB(t)
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdRegion([]string{"--db", dbPath})
	})

	assert.Contains(t, output, "Total: 0 regions")
}

// ============================================================================
// Dump decode tests
// ============================================================================

func TestCmdDumpWriteCFDecode(t *testing.T) {
	db := newTestDB(t)

	write := &txntypes.Write{
		WriteType:  txntypes.WriteTypePut,
		StartTS:    10,
		ShortValue: []byte("Hello"),
	}
	key := mvcc.EncodeKey([]byte("mykey"), 15)
	require.NoError(t, db.eng.Put(cfnames.CFWrite, key, write.Marshal()))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdDump([]string{"--db", dbPath, "--cf", "write", "--decode"})
	})

	assert.Contains(t, output, "[MVCC Write]")
	assert.Contains(t, output, "CommitTS: 15")
	assert.Contains(t, output, "StartTS: 10")
	assert.Contains(t, output, "Put")
}

func TestCmdDumpLockCFDecode(t *testing.T) {
	db := newTestDB(t)

	lock := &txntypes.Lock{
		LockType: txntypes.LockTypePut,
		Primary:  []byte("pk"),
		StartTS:  20,
		TTL:      3000,
	}
	key := mvcc.EncodeLockKey([]byte("lockedkey"))
	require.NoError(t, db.eng.Put(cfnames.CFLock, key, lock.Marshal()))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdDump([]string{"--db", dbPath, "--cf", "lock", "--decode"})
	})

	assert.Contains(t, output, "[MVCC Lock]")
	assert.Contains(t, output, "StartTS: 20")
	assert.Contains(t, output, "TTL: 3000")
}

func TestCmdDumpDefaultCFDecode(t *testing.T) {
	db := newTestDB(t)

	key := mvcc.EncodeKey([]byte("valkey"), 5)
	require.NoError(t, db.eng.Put(cfnames.CFDefault, key, []byte("myvalue")))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdDump([]string{"--db", dbPath, "--cf", "default", "--decode"})
	})

	assert.Contains(t, output, "[MVCC Default]")
	assert.Contains(t, output, "StartTS: 5")
	assert.Contains(t, output, fmt.Sprintf("%d bytes", len("myvalue")))
}

func TestCmdDumpRaftCFDecode(t *testing.T) {
	db := newTestDB(t)

	state := &raft_serverpb.RegionLocalState{
		State:  raft_serverpb.PeerState_Normal,
		Region: &metapb.Region{Id: 7},
	}
	data, err := state.Marshal()
	require.NoError(t, err)
	require.NoError(t, db.eng.Put(cfnames.CFRaft, keys.RegionStateKey(7), data))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdDump([]string{"--db", dbPath, "--cf", "raft", "--decode"})
	})

	assert.Contains(t, output, "[Raft RegionState]")
	assert.Contains(t, output, "RegionID: 7")
}

func TestCmdDumpRawFallback(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.eng.Put(cfnames.CFDefault, []byte("rawkey"), []byte("rawval")))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdDump([]string{"--db", dbPath, "--cf", "default"})
	})

	// Raw hex output without --decode.
	assert.NotContains(t, output, "[MVCC")
}

// ============================================================================
// Compact command tests
// ============================================================================

func TestCmdCompactAll(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.eng.Put(cfnames.CFDefault, []byte("k"), []byte("v")))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdCompact([]string{"--db", dbPath})
	})

	assert.Contains(t, output, "Compaction completed successfully.")
}

func TestCmdCompactSingleCF(t *testing.T) {
	db := newTestDB(t)
	require.NoError(t, db.eng.Put(cfnames.CFDefault, []byte("k"), []byte("v")))
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdCompact([]string{"--db", dbPath, "--cf", "default"})
	})

	assert.Contains(t, output, "Compacting CF default")
	assert.Contains(t, output, "Compaction completed successfully.")
}

func TestCmdCompactFlushOnly(t *testing.T) {
	db := newTestDB(t)
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdCompact([]string{"--db", dbPath, "--flush-only"})
	})

	assert.Contains(t, output, "WAL flushed successfully.")
}

func TestCmdCompactEmptyDB(t *testing.T) {
	db := newTestDB(t)
	dbPath := db.closeAndPath(t)

	output := captureOutput(func() {
		cmdCompact([]string{"--db", dbPath})
	})

	assert.Contains(t, output, "Compaction completed successfully.")
}
