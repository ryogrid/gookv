package mvcc

import (
	"path/filepath"
	"testing"

	"github.com/ryogrid/gookv/internal/engine/rocks"
	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/pkg/cfnames"
	"github.com/ryogrid/gookv/pkg/txntypes"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// newScanTestEngine creates a temporary engine for scanner tests.
func newScanTestEngine(t *testing.T) traits.KvEngine {
	t.Helper()
	dir := t.TempDir()
	e, err := rocks.Open(filepath.Join(dir, "scan-test-db"))
	require.NoError(t, err)
	t.Cleanup(func() { e.Close() })
	return e
}

// putWrite writes a write record for key at commitTS with the given startTS.
func putWrite(t *testing.T, eng traits.KvEngine, userKey []byte, commitTS, startTS txntypes.TimeStamp, wt txntypes.WriteType, shortValue []byte) {
	t.Helper()
	w := &txntypes.Write{
		WriteType:  wt,
		StartTS:    startTS,
		ShortValue: shortValue,
	}
	key := EncodeKey(userKey, commitTS)
	require.NoError(t, eng.Put(cfnames.CFWrite, key, w.Marshal()))
}

// putDefault writes a value to CF_DEFAULT.
func putDefault(t *testing.T, eng traits.KvEngine, userKey []byte, startTS txntypes.TimeStamp, value []byte) {
	t.Helper()
	key := EncodeKey(userKey, startTS)
	require.NoError(t, eng.Put(cfnames.CFDefault, key, value))
}

// putLock writes a lock record for key.
func putLock(t *testing.T, eng traits.KvEngine, userKey []byte, lockType txntypes.LockType, startTS txntypes.TimeStamp, primary []byte) {
	t.Helper()
	lock := &txntypes.Lock{
		LockType: lockType,
		Primary:  primary,
		StartTS:  startTS,
		TTL:      3000,
	}
	key := EncodeLockKey(userKey)
	require.NoError(t, eng.Put(cfnames.CFLock, key, lock.Marshal()))
}

// collectScan runs a forward scan and returns all key-value pairs.
func collectScan(t *testing.T, eng traits.KvEngine, readTS txntypes.TimeStamp, lower, upper Key, level IsolationLevel) ([]Key, [][]byte, error) {
	t.Helper()
	snap := eng.NewSnapshot()
	defer snap.Close()

	cfg := ScannerConfig{
		Snapshot:       snap,
		ReadTS:         readTS,
		IsolationLevel: level,
		LowerBound:     lower,
		UpperBound:     upper,
	}
	scanner := NewScanner(cfg)
	defer scanner.Close()

	var keys []Key
	var values [][]byte
	for {
		k, v, err := scanner.Next()
		if err != nil {
			return keys, values, err
		}
		if k == nil {
			break
		}
		keys = append(keys, append(Key(nil), k...))
		values = append(values, v)
	}
	return keys, values, nil
}

func TestForwardScanBasic(t *testing.T) {
	eng := newScanTestEngine(t)

	// 10 keys, each with one Put version at commitTS=5.
	for i := byte(0); i < 10; i++ {
		key := []byte{i}
		putWrite(t, eng, key, 5, 3, txntypes.WriteTypePut, []byte{i + 100})
	}

	keys, values, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 10)
	for i := 0; i < 10; i++ {
		assert.Equal(t, []byte{byte(i)}, keys[i])
		assert.Equal(t, []byte{byte(i + 100)}, values[i])
	}
}

func TestForwardScanMultipleVersions(t *testing.T) {
	eng := newScanTestEngine(t)

	key := []byte("key1")
	// Versions at commitTS=3,5,7,9,11. Read at ts=8 should see commitTS=7 (startTS=6).
	putWrite(t, eng, key, 3, 2, txntypes.WriteTypePut, []byte("v3"))
	putWrite(t, eng, key, 5, 4, txntypes.WriteTypePut, []byte("v5"))
	putWrite(t, eng, key, 7, 6, txntypes.WriteTypePut, []byte("v7"))
	putWrite(t, eng, key, 9, 8, txntypes.WriteTypePut, []byte("v9"))
	putWrite(t, eng, key, 11, 10, txntypes.WriteTypePut, []byte("v11"))

	keys, values, err := collectScan(t, eng, 8, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	require.Len(t, keys, 1)
	assert.Equal(t, key, keys[0])
	assert.Equal(t, []byte("v7"), values[0])
}

func TestForwardScanDeletes(t *testing.T) {
	eng := newScanTestEngine(t)

	// key "a": Put at commitTS=5
	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	// key "b": Put at commitTS=5, then Delete at commitTS=8
	putWrite(t, eng, []byte("b"), 5, 3, txntypes.WriteTypePut, []byte("vb"))
	putWrite(t, eng, []byte("b"), 8, 7, txntypes.WriteTypeDelete, nil)
	// key "c": Put at commitTS=5
	putWrite(t, eng, []byte("c"), 5, 3, txntypes.WriteTypePut, []byte("vc"))

	keys, values, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 2) // "b" is deleted
	assert.Equal(t, []byte("a"), keys[0])
	assert.Equal(t, []byte("va"), values[0])
	assert.Equal(t, []byte("c"), keys[1])
	assert.Equal(t, []byte("vc"), values[1])
}

func TestForwardScanLockConflict(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putLock(t, eng, []byte("a"), txntypes.LockTypePut, 8, []byte("primary"))

	_, _, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	assert.ErrorIs(t, err, ErrKeyIsLocked)
}

func TestForwardScanBypassLocks(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putLock(t, eng, []byte("a"), txntypes.LockTypePut, 8, []byte("primary"))

	snap := eng.NewSnapshot()
	defer snap.Close()

	cfg := ScannerConfig{
		Snapshot:       snap,
		ReadTS:         10,
		IsolationLevel: IsolationLevelSI,
		BypassLocks:    map[txntypes.TimeStamp]bool{8: true},
	}
	scanner := NewScanner(cfg)
	defer scanner.Close()

	key, value, err := scanner.Next()
	require.NoError(t, err)
	assert.Equal(t, []byte("a"), key)
	assert.Equal(t, []byte("va"), value)
}

func TestForwardScanPessimisticLock(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putLock(t, eng, []byte("a"), txntypes.LockTypePessimistic, 8, []byte("primary"))

	keys, values, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	assert.Equal(t, []byte("va"), values[0])
}

func TestForwardScanRCIsolation(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putLock(t, eng, []byte("a"), txntypes.LockTypePut, 8, []byte("primary"))

	// RC isolation should skip all lock checking.
	keys, values, err := collectScan(t, eng, 10, nil, nil, IsolationLevelRC)
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	assert.Equal(t, []byte("va"), values[0])
}

func TestForwardScanKeyOnly(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putWrite(t, eng, []byte("b"), 5, 3, txntypes.WriteTypePut, []byte("vb"))

	snap := eng.NewSnapshot()
	defer snap.Close()

	cfg := ScannerConfig{
		Snapshot:       snap,
		ReadTS:         10,
		IsolationLevel: IsolationLevelSI,
		KeyOnly:        true,
	}
	scanner := NewScanner(cfg)
	defer scanner.Close()

	key, value, err := scanner.Next()
	require.NoError(t, err)
	assert.Equal(t, []byte("a"), key)
	assert.Nil(t, value) // KeyOnly mode

	key, value, err = scanner.Next()
	require.NoError(t, err)
	assert.Equal(t, []byte("b"), key)
	assert.Nil(t, value)
}

func TestForwardScanRangeBounds(t *testing.T) {
	eng := newScanTestEngine(t)

	for _, k := range []string{"a", "b", "c", "d", "e"} {
		putWrite(t, eng, []byte(k), 5, 3, txntypes.WriteTypePut, []byte("v-"+k))
	}

	keys, _, err := collectScan(t, eng, 10, []byte("b"), []byte("d"), IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 2)
	assert.Equal(t, []byte("b"), keys[0])
	assert.Equal(t, []byte("c"), keys[1])
}

func TestForwardScanRollbackRecords(t *testing.T) {
	eng := newScanTestEngine(t)

	// Key has: Rollback at commitTS=7, Put at commitTS=5.
	putWrite(t, eng, []byte("a"), 7, 6, txntypes.WriteTypeRollback, nil)
	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))

	keys, values, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	assert.Equal(t, []byte("va"), values[0])
}

func TestForwardScanShortAndLongValues(t *testing.T) {
	eng := newScanTestEngine(t)

	// Short value (inlined in write record).
	putWrite(t, eng, []byte("short"), 5, 3, txntypes.WriteTypePut, []byte("sv"))

	// Long value (stored in CF_DEFAULT).
	putWrite(t, eng, []byte("long"), 5, 3, txntypes.WriteTypePut, nil)
	putDefault(t, eng, []byte("long"), 3, []byte("long-value-data"))

	keys, values, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	require.Len(t, keys, 2)
	assert.Equal(t, []byte("long"), keys[0])
	assert.Equal(t, []byte("long-value-data"), values[0])
	assert.Equal(t, []byte("short"), keys[1])
	assert.Equal(t, []byte("sv"), values[1])
}

func TestForwardScanEmptyRange(t *testing.T) {
	eng := newScanTestEngine(t)

	keys, _, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 0)
}

func TestForwardScanStatistics(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putWrite(t, eng, []byte("b"), 5, 3, txntypes.WriteTypePut, []byte("vb"))
	putWrite(t, eng, []byte("c"), 5, 3, txntypes.WriteTypePut, []byte("vc"))

	snap := eng.NewSnapshot()
	defer snap.Close()

	cfg := ScannerConfig{
		Snapshot:       snap,
		ReadTS:         10,
		IsolationLevel: IsolationLevelSI,
	}
	scanner := NewScanner(cfg)
	defer scanner.Close()

	for {
		k, _, err := scanner.Next()
		require.NoError(t, err)
		if k == nil {
			break
		}
	}

	stats := scanner.TakeStatistics()
	assert.Equal(t, int64(3), stats.ProcessedKeys)
	assert.GreaterOrEqual(t, stats.ScannedKeys, int64(3))
}

func TestBackwardScanBasic(t *testing.T) {
	eng := newScanTestEngine(t)

	for _, k := range []string{"a", "b", "c"} {
		putWrite(t, eng, []byte(k), 5, 3, txntypes.WriteTypePut, []byte("v-"+k))
	}

	snap := eng.NewSnapshot()
	defer snap.Close()

	cfg := ScannerConfig{
		Snapshot:       snap,
		ReadTS:         10,
		Desc:           true,
		IsolationLevel: IsolationLevelSI,
	}
	scanner := NewScanner(cfg)
	defer scanner.Close()

	var keys []string
	for {
		k, _, err := scanner.Next()
		require.NoError(t, err)
		if k == nil {
			break
		}
		keys = append(keys, string(k))
	}

	assert.Equal(t, []string{"c", "b", "a"}, keys)
}

func TestBackwardScanMultipleVersions(t *testing.T) {
	eng := newScanTestEngine(t)

	key := []byte("k")
	putWrite(t, eng, key, 3, 2, txntypes.WriteTypePut, []byte("v3"))
	putWrite(t, eng, key, 7, 6, txntypes.WriteTypePut, []byte("v7"))
	putWrite(t, eng, key, 11, 10, txntypes.WriteTypePut, []byte("v11"))

	snap := eng.NewSnapshot()
	defer snap.Close()

	cfg := ScannerConfig{
		Snapshot:       snap,
		ReadTS:         8,
		Desc:           true,
		IsolationLevel: IsolationLevelSI,
	}
	scanner := NewScanner(cfg)
	defer scanner.Close()

	k, v, err := scanner.Next()
	require.NoError(t, err)
	assert.Equal(t, key, k)
	assert.Equal(t, []byte("v7"), v)
}

func TestBackwardScanLockConflict(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putLock(t, eng, []byte("a"), txntypes.LockTypePut, 8, []byte("primary"))

	snap := eng.NewSnapshot()
	defer snap.Close()

	cfg := ScannerConfig{
		Snapshot:       snap,
		ReadTS:         10,
		Desc:           true,
		IsolationLevel: IsolationLevelSI,
	}
	scanner := NewScanner(cfg)
	defer scanner.Close()

	_, _, err := scanner.Next()
	assert.ErrorIs(t, err, ErrKeyIsLocked)
}

// Test that scan returns only the latest visible version per user key (deduplication).
func TestForwardScanDeduplication(t *testing.T) {
	eng := newScanTestEngine(t)

	// Key "a" has 3 Put versions. Only the latest visible should be returned.
	putWrite(t, eng, []byte("a"), 3, 2, txntypes.WriteTypePut, []byte("v3"))
	putWrite(t, eng, []byte("a"), 5, 4, txntypes.WriteTypePut, []byte("v5"))
	putWrite(t, eng, []byte("a"), 7, 6, txntypes.WriteTypePut, []byte("v7"))

	keys, values, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 1)
	assert.Equal(t, []byte("v7"), values[0])
}

// Test lock with startTS > readTS should not conflict.
func TestForwardScanLockFutureTS(t *testing.T) {
	eng := newScanTestEngine(t)

	putWrite(t, eng, []byte("a"), 5, 3, txntypes.WriteTypePut, []byte("va"))
	putLock(t, eng, []byte("a"), txntypes.LockTypePut, 15, []byte("primary")) // future lock

	keys, _, err := collectScan(t, eng, 10, nil, nil, IsolationLevelSI)
	require.NoError(t, err)
	assert.Len(t, keys, 1)
}
