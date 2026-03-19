package raftstore

import (
	"sync/atomic"
	"testing"
	"time"

	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"

	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/pkg/cfnames"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMarshalUnmarshalSnapshotData(t *testing.T) {
	sd := &SnapshotData{
		RegionID: 42,
		Version:  SnapshotVersion,
		CFFiles: []SnapshotCFFile{
			{
				CF: cfnames.CFDefault,
				KVPairs: []SnapKVPair{
					{Key: []byte("key1"), Value: []byte("val1")},
					{Key: []byte("key2"), Value: []byte("val2")},
				},
			},
			{
				CF:      cfnames.CFWrite,
				KVPairs: []SnapKVPair{},
			},
		},
	}
	// Set checksums.
	for i := range sd.CFFiles {
		sd.CFFiles[i].Checksum = ComputeCFChecksum(sd.CFFiles[i].KVPairs)
	}

	data, err := MarshalSnapshotData(sd)
	require.NoError(t, err)

	decoded, err := UnmarshalSnapshotData(data)
	require.NoError(t, err)

	assert.Equal(t, uint64(42), decoded.RegionID)
	assert.Equal(t, uint64(SnapshotVersion), decoded.Version)
	require.Len(t, decoded.CFFiles, 2)

	assert.Equal(t, cfnames.CFDefault, decoded.CFFiles[0].CF)
	require.Len(t, decoded.CFFiles[0].KVPairs, 2)
	assert.Equal(t, []byte("key1"), decoded.CFFiles[0].KVPairs[0].Key)
	assert.Equal(t, []byte("val1"), decoded.CFFiles[0].KVPairs[0].Value)
	assert.Equal(t, []byte("key2"), decoded.CFFiles[0].KVPairs[1].Key)

	assert.Equal(t, cfnames.CFWrite, decoded.CFFiles[1].CF)
	assert.Len(t, decoded.CFFiles[1].KVPairs, 0)
}

func TestComputeCFChecksum(t *testing.T) {
	pairs := []SnapKVPair{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("b"), Value: []byte("2")},
	}

	c1 := ComputeCFChecksum(pairs)
	c2 := ComputeCFChecksum(pairs)
	assert.Equal(t, c1, c2, "checksum should be deterministic")

	// Different data -> different checksum.
	pairs2 := []SnapKVPair{
		{Key: []byte("a"), Value: []byte("1")},
		{Key: []byte("c"), Value: []byte("3")},
	}
	c3 := ComputeCFChecksum(pairs2)
	assert.NotEqual(t, c1, c3)

	// Empty pairs.
	c4 := ComputeCFChecksum(nil)
	assert.NotEqual(t, c4, c1)
}

func TestGenerateSnapshotData(t *testing.T) {
	engine := newTestEngine(t)

	// Write some data.
	require.NoError(t, engine.Put(cfnames.CFDefault, []byte("dk1"), []byte("dv1")))
	require.NoError(t, engine.Put(cfnames.CFDefault, []byte("dk2"), []byte("dv2")))
	require.NoError(t, engine.Put(cfnames.CFWrite, []byte("wk1"), []byte("wv1")))

	sd, err := GenerateSnapshotData(engine, 1, nil, nil)
	require.NoError(t, err)

	assert.Equal(t, uint64(1), sd.RegionID)
	assert.Equal(t, uint64(SnapshotVersion), sd.Version)
	require.Len(t, sd.CFFiles, 3) // default, lock, write

	// Default CF should have 2 keys.
	defaultCF := sd.CFFiles[0]
	assert.Equal(t, cfnames.CFDefault, defaultCF.CF)
	assert.Len(t, defaultCF.KVPairs, 2)

	// Lock CF should be empty.
	lockCF := sd.CFFiles[1]
	assert.Equal(t, cfnames.CFLock, lockCF.CF)
	assert.Len(t, lockCF.KVPairs, 0)

	// Write CF should have 1 key.
	writeCF := sd.CFFiles[2]
	assert.Equal(t, cfnames.CFWrite, writeCF.CF)
	assert.Len(t, writeCF.KVPairs, 1)
}

func TestApplySnapshotData(t *testing.T) {
	engine := newTestEngine(t)

	// Write some initial data.
	require.NoError(t, engine.Put(cfnames.CFDefault, []byte("old1"), []byte("val")))

	// Create snapshot data.
	sd := &SnapshotData{
		RegionID: 1,
		Version:  SnapshotVersion,
		CFFiles: []SnapshotCFFile{
			{
				CF: cfnames.CFDefault,
				KVPairs: []SnapKVPair{
					{Key: []byte("new1"), Value: []byte("newval1")},
					{Key: []byte("new2"), Value: []byte("newval2")},
				},
			},
			{
				CF:      cfnames.CFLock,
				KVPairs: nil,
			},
			{
				CF:      cfnames.CFWrite,
				KVPairs: nil,
			},
		},
	}
	for i := range sd.CFFiles {
		sd.CFFiles[i].Checksum = ComputeCFChecksum(sd.CFFiles[i].KVPairs)
	}

	err := ApplySnapshotData(engine, sd, nil, nil)
	require.NoError(t, err)

	// New keys should exist.
	val, err := engine.Get(cfnames.CFDefault, []byte("new1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("newval1"), val)

	val, err = engine.Get(cfnames.CFDefault, []byte("new2"))
	require.NoError(t, err)
	assert.Equal(t, []byte("newval2"), val)
}

func TestApplySnapshotData_ChecksumMismatch(t *testing.T) {
	engine := newTestEngine(t)

	sd := &SnapshotData{
		RegionID: 1,
		Version:  SnapshotVersion,
		CFFiles: []SnapshotCFFile{
			{
				CF:       cfnames.CFDefault,
				KVPairs:  []SnapKVPair{{Key: []byte("k"), Value: []byte("v")}},
				Checksum: 12345, // Wrong checksum.
			},
		},
	}

	err := ApplySnapshotData(engine, sd, nil, nil)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "checksum mismatch")
}

func TestPeerStorageApplySnapshot(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPeerStorage(1, engine)

	// Create snapshot data.
	sd := &SnapshotData{
		RegionID: 1,
		Version:  SnapshotVersion,
		CFFiles: []SnapshotCFFile{
			{CF: cfnames.CFDefault, KVPairs: []SnapKVPair{{Key: []byte("k1"), Value: []byte("v1")}}},
			{CF: cfnames.CFLock, KVPairs: nil},
			{CF: cfnames.CFWrite, KVPairs: nil},
		},
	}
	for i := range sd.CFFiles {
		sd.CFFiles[i].Checksum = ComputeCFChecksum(sd.CFFiles[i].KVPairs)
	}

	data, err := MarshalSnapshotData(sd)
	require.NoError(t, err)

	snap := raftpb.Snapshot{
		Data: data,
		Metadata: raftpb.SnapshotMetadata{
			Index: 100,
			Term:  5,
		},
	}

	err = s.ApplySnapshot(snap)
	require.NoError(t, err)

	// Check state was updated.
	assert.Equal(t, uint64(100), s.AppliedIndex())
	assert.Equal(t, uint64(100), s.TruncatedIndex())
	assert.Equal(t, uint64(5), s.TruncatedTerm())

	// Verify data was written.
	val, err := engine.Get(cfnames.CFDefault, []byte("k1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("v1"), val)
}

func TestPeerStorageApplySnapshot_StaleSnapshot(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPeerStorage(1, engine)

	// Set truncated index higher.
	s.SetApplyState(ApplyState{
		AppliedIndex:   50,
		TruncatedIndex: 50,
		TruncatedTerm:  3,
	})

	// Apply a stale snapshot with lower index.
	snap := raftpb.Snapshot{
		Data: nil,
		Metadata: raftpb.SnapshotMetadata{
			Index: 30,
			Term:  2,
		},
	}

	err := s.ApplySnapshot(snap)
	require.NoError(t, err)
	// State should not change.
	assert.Equal(t, uint64(50), s.TruncatedIndex())
}

func TestSnapWorker(t *testing.T) {
	engine := newTestEngine(t)

	// Write some data.
	require.NoError(t, engine.Put(cfnames.CFDefault, []byte("k1"), []byte("v1")))
	require.NoError(t, engine.Put(cfnames.CFWrite, []byte("wk1"), []byte("wv1")))

	taskCh := make(chan GenSnapTask, 1)
	stopCh := make(chan struct{})
	worker := NewSnapWorker(engine, taskCh, stopCh)

	go worker.Run()

	resultCh := make(chan GenSnapResult, 1)
	taskCh <- GenSnapTask{
		RegionID: 1,
		SnapKey: SnapKey{
			RegionID: 1,
			Term:     5,
			Index:    100,
		},
		ResultCh: resultCh,
	}

	var result GenSnapResult
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("snap worker timed out")
	}

	close(stopCh)

	require.NoError(t, result.Err)
	assert.Equal(t, uint64(100), result.Snapshot.Metadata.Index)
	assert.Equal(t, uint64(5), result.Snapshot.Metadata.Term)
	assert.True(t, len(result.Snapshot.Data) > 0)

	// Verify the snapshot data can be deserialized.
	sd, err := UnmarshalSnapshotData(result.Snapshot.Data)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), sd.RegionID)
}

func TestSnapWorker_Cancelled(t *testing.T) {
	engine := newTestEngine(t)

	taskCh := make(chan GenSnapTask, 1)
	stopCh := make(chan struct{})
	worker := NewSnapWorker(engine, taskCh, stopCh)

	go worker.Run()

	resultCh := make(chan GenSnapResult, 1)
	canceled := &atomic.Bool{}
	canceled.Store(true) // Pre-cancel.

	taskCh <- GenSnapTask{
		RegionID: 1,
		SnapKey:  SnapKey{RegionID: 1, Term: 1, Index: 1},
		Canceled: canceled,
		ResultCh: resultCh,
	}

	var result GenSnapResult
	select {
	case result = <-resultCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out")
	}

	close(stopCh)

	assert.Error(t, result.Err)
	assert.Contains(t, result.Err.Error(), "cancelled")
}

func TestRequestSnapshot_StateTransitions(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPeerStorage(1, engine)

	require.NoError(t, engine.Put(cfnames.CFDefault, []byte("data"), []byte("value")))

	taskCh := make(chan GenSnapTask, 1)
	stopCh := make(chan struct{})
	worker := NewSnapWorker(engine, taskCh, stopCh)
	go worker.Run()
	defer close(stopCh)

	// First call: should return ErrSnapshotTemporarilyUnavailable.
	_, err := s.RequestSnapshot(taskCh, nil)
	assert.Equal(t, raft.ErrSnapshotTemporarilyUnavailable, err)

	// Wait for worker to produce result.
	time.Sleep(200 * time.Millisecond)

	// Second call: should return the snapshot.
	snap, err := s.RequestSnapshot(taskCh, nil)
	require.NoError(t, err)
	assert.True(t, snap.Metadata.Index > 0 || len(snap.Data) > 0)
}

func TestCancelGeneratingSnap(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPeerStorage(1, engine)

	// Simulate generating state.
	s.mu.Lock()
	s.snapState = SnapStateGenerating
	canceled := &atomic.Bool{}
	s.snapCanceled = canceled
	s.snapReceiver = make(chan GenSnapResult, 1)
	s.mu.Unlock()

	s.CancelGeneratingSnap()

	s.mu.RLock()
	assert.Equal(t, SnapStateRelax, s.snapState)
	assert.True(t, canceled.Load())
	s.mu.RUnlock()
}

func TestSnapshotRoundTrip(t *testing.T) {
	// Generate snapshot from one engine, apply to another.
	srcEngine := newTestEngine(t)
	dstEngine := newTestEngine(t)

	// Write data to source.
	require.NoError(t, srcEngine.Put(cfnames.CFDefault, []byte("key1"), []byte("val1")))
	require.NoError(t, srcEngine.Put(cfnames.CFDefault, []byte("key2"), []byte("val2")))
	require.NoError(t, srcEngine.Put(cfnames.CFWrite, []byte("wk1"), []byte("wv1")))

	// Generate snapshot.
	sd, err := GenerateSnapshotData(srcEngine, 1, nil, nil)
	require.NoError(t, err)

	data, err := MarshalSnapshotData(sd)
	require.NoError(t, err)

	// Apply to destination.
	dstStorage := NewPeerStorage(1, dstEngine)
	snap := raftpb.Snapshot{
		Data:     data,
		Metadata: raftpb.SnapshotMetadata{Index: 100, Term: 5},
	}

	err = dstStorage.ApplySnapshot(snap)
	require.NoError(t, err)

	// Verify data in destination.
	val, err := dstEngine.Get(cfnames.CFDefault, []byte("key1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("val1"), val)

	val, err = dstEngine.Get(cfnames.CFDefault, []byte("key2"))
	require.NoError(t, err)
	assert.Equal(t, []byte("val2"), val)

	val, err = dstEngine.Get(cfnames.CFWrite, []byte("wk1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("wv1"), val)

	// Verify destination doesn't have lock data (empty).
	_, err = dstEngine.Get(cfnames.CFLock, []byte("anything"))
	assert.Equal(t, traits.ErrNotFound, err)
}
