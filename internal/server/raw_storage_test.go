package server

import (
	"path/filepath"
	"testing"

	"github.com/ryogrid/gookvs/internal/engine/rocks"
	"github.com/ryogrid/gookvs/pkg/cfnames"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestRawStorage(t *testing.T) *RawStorage {
	t.Helper()
	dir := t.TempDir()
	eng, err := rocks.Open(filepath.Join(dir, "test-db"))
	require.NoError(t, err)
	t.Cleanup(func() { eng.Close() })
	return NewRawStorage(eng)
}

func TestRawGet_Found(t *testing.T) {
	rs := newTestRawStorage(t)
	require.NoError(t, rs.Put("", []byte("k1"), []byte("v1")))

	val, err := rs.Get("", []byte("k1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v1"), val)
}

func TestRawGet_NotFound(t *testing.T) {
	rs := newTestRawStorage(t)
	val, err := rs.Get("", []byte("missing"))
	require.NoError(t, err)
	assert.Nil(t, val)
}

func TestRawPut_Overwrite(t *testing.T) {
	rs := newTestRawStorage(t)
	require.NoError(t, rs.Put("", []byte("k1"), []byte("v1")))
	require.NoError(t, rs.Put("", []byte("k1"), []byte("v2")))

	val, err := rs.Get("", []byte("k1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v2"), val)
}

func TestRawDelete(t *testing.T) {
	rs := newTestRawStorage(t)
	require.NoError(t, rs.Put("", []byte("k1"), []byte("v1")))
	require.NoError(t, rs.Delete("", []byte("k1")))

	val, err := rs.Get("", []byte("k1"))
	require.NoError(t, err)
	assert.Nil(t, val)
}

func TestRawScan_Forward(t *testing.T) {
	rs := newTestRawStorage(t)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		require.NoError(t, rs.Put("", []byte(k), []byte("v-"+k)))
	}

	pairs, err := rs.Scan("", []byte("b"), []byte("d"), 10, false, false)
	require.NoError(t, err)
	assert.Len(t, pairs, 2)
	assert.Equal(t, []byte("b"), pairs[0].Key)
	assert.Equal(t, []byte("c"), pairs[1].Key)
}

func TestRawScan_Limit(t *testing.T) {
	rs := newTestRawStorage(t)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		require.NoError(t, rs.Put("", []byte(k), []byte("v-"+k)))
	}

	pairs, err := rs.Scan("", nil, nil, 3, false, false)
	require.NoError(t, err)
	assert.Len(t, pairs, 3)
}

func TestRawScan_KeyOnly(t *testing.T) {
	rs := newTestRawStorage(t)
	require.NoError(t, rs.Put("", []byte("k1"), []byte("v1")))

	pairs, err := rs.Scan("", nil, nil, 10, true, false)
	require.NoError(t, err)
	require.Len(t, pairs, 1)
	assert.Equal(t, []byte("k1"), pairs[0].Key)
	assert.Nil(t, pairs[0].Value)
}

func TestRawScan_Reverse(t *testing.T) {
	rs := newTestRawStorage(t)
	for _, k := range []string{"a", "b", "c"} {
		require.NoError(t, rs.Put("", []byte(k), []byte("v-"+k)))
	}

	pairs, err := rs.Scan("", nil, nil, 10, false, true)
	require.NoError(t, err)
	require.Len(t, pairs, 3)
	assert.Equal(t, []byte("c"), pairs[0].Key)
	assert.Equal(t, []byte("b"), pairs[1].Key)
	assert.Equal(t, []byte("a"), pairs[2].Key)
}

func TestRawBatchGet(t *testing.T) {
	rs := newTestRawStorage(t)
	require.NoError(t, rs.Put("", []byte("a"), []byte("1")))
	require.NoError(t, rs.Put("", []byte("c"), []byte("3")))

	pairs, err := rs.BatchGet("", [][]byte{[]byte("a"), []byte("b"), []byte("c")})
	require.NoError(t, err)
	assert.Len(t, pairs, 2) // "b" is missing
	assert.Equal(t, []byte("a"), pairs[0].Key)
	assert.Equal(t, []byte("c"), pairs[1].Key)
}

func TestRawBatchPut(t *testing.T) {
	rs := newTestRawStorage(t)
	pairs := []KvPair{
		{Key: []byte("k1"), Value: []byte("v1")},
		{Key: []byte("k2"), Value: []byte("v2")},
	}
	require.NoError(t, rs.BatchPut("", pairs))

	val, err := rs.Get("", []byte("k1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v1"), val)

	val, err = rs.Get("", []byte("k2"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v2"), val)
}

func TestRawBatchDelete(t *testing.T) {
	rs := newTestRawStorage(t)
	require.NoError(t, rs.Put("", []byte("a"), []byte("1")))
	require.NoError(t, rs.Put("", []byte("b"), []byte("2")))
	require.NoError(t, rs.Put("", []byte("c"), []byte("3")))

	require.NoError(t, rs.BatchDelete("", [][]byte{[]byte("a"), []byte("c")}))

	val, err := rs.Get("", []byte("a"))
	require.NoError(t, err)
	assert.Nil(t, val)

	val, err = rs.Get("", []byte("b"))
	require.NoError(t, err)
	assert.Equal(t, []byte("2"), val)

	val, err = rs.Get("", []byte("c"))
	require.NoError(t, err)
	assert.Nil(t, val)
}

func TestRawDeleteRange(t *testing.T) {
	rs := newTestRawStorage(t)
	for _, k := range []string{"a", "b", "c", "d", "e"} {
		require.NoError(t, rs.Put("", []byte(k), []byte("v")))
	}

	require.NoError(t, rs.DeleteRange("", []byte("b"), []byte("d")))

	// a and d,e should exist; b,c should not.
	val, err := rs.Get("", []byte("a"))
	require.NoError(t, err)
	assert.NotNil(t, val)

	val, err = rs.Get("", []byte("b"))
	require.NoError(t, err)
	assert.Nil(t, val)

	val, err = rs.Get("", []byte("c"))
	require.NoError(t, err)
	assert.Nil(t, val)

	val, err = rs.Get("", []byte("d"))
	require.NoError(t, err)
	assert.NotNil(t, val)
}

func TestRawCFSupport(t *testing.T) {
	rs := newTestRawStorage(t)

	// Write to different CFs.
	require.NoError(t, rs.Put(cfnames.CFDefault, []byte("k"), []byte("default")))
	require.NoError(t, rs.Put(cfnames.CFWrite, []byte("k"), []byte("write")))

	// Read from specific CFs.
	val, err := rs.Get(cfnames.CFDefault, []byte("k"))
	require.NoError(t, err)
	assert.Equal(t, []byte("default"), val)

	val, err = rs.Get(cfnames.CFWrite, []byte("k"))
	require.NoError(t, err)
	assert.Equal(t, []byte("write"), val)
}

func TestRawResolveCFEmpty(t *testing.T) {
	rs := newTestRawStorage(t)

	// Empty CF should default to CF_DEFAULT.
	require.NoError(t, rs.Put("", []byte("k"), []byte("v")))
	val, err := rs.Get(cfnames.CFDefault, []byte("k"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v"), val)
}

func TestRawPutModify(t *testing.T) {
	rs := newTestRawStorage(t)
	m := rs.PutModify("", []byte("k"), []byte("v"))
	assert.Equal(t, cfnames.CFDefault, m.CF)
	assert.Equal(t, []byte("k"), m.Key)
	assert.Equal(t, []byte("v"), m.Value)
}

func TestRawDeleteModify(t *testing.T) {
	rs := newTestRawStorage(t)
	m := rs.DeleteModify("", []byte("k"))
	assert.Equal(t, cfnames.CFDefault, m.CF)
	assert.Equal(t, []byte("k"), m.Key)
}
