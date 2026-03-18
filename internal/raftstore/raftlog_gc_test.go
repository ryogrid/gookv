package raftstore

import (
	"testing"
	"time"

	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"

	"github.com/ryogrid/gookvs/pkg/cfnames"
	"github.com/ryogrid/gookvs/pkg/keys"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExecCompactLog_Basic(t *testing.T) {
	state := &ApplyState{
		AppliedIndex:   100,
		TruncatedIndex: 10,
		TruncatedTerm:  1,
	}

	req := CompactLogRequest{
		CompactIndex: 50,
		CompactTerm:  3,
	}

	result, err := execCompactLog(state, req)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, uint64(50), result.TruncatedIndex)
	assert.Equal(t, uint64(3), result.TruncatedTerm)
	assert.Equal(t, uint64(11), result.FirstIndex) // old TruncatedIndex + 1
	assert.Equal(t, uint64(50), state.TruncatedIndex)
	assert.Equal(t, uint64(3), state.TruncatedTerm)
}

func TestExecCompactLog_AlreadyCompacted(t *testing.T) {
	state := &ApplyState{
		AppliedIndex:   100,
		TruncatedIndex: 50,
		TruncatedTerm:  3,
	}

	req := CompactLogRequest{
		CompactIndex: 30, // Already past this
		CompactTerm:  2,
	}

	result, err := execCompactLog(state, req)
	require.NoError(t, err)
	assert.Nil(t, result, "should be no-op for already compacted index")
	assert.Equal(t, uint64(50), state.TruncatedIndex, "state should not change")
}

func TestExecCompactLog_Idempotent(t *testing.T) {
	state := &ApplyState{
		AppliedIndex:   100,
		TruncatedIndex: 10,
		TruncatedTerm:  1,
	}

	req := CompactLogRequest{CompactIndex: 50, CompactTerm: 3}

	result1, err := execCompactLog(state, req)
	require.NoError(t, err)
	require.NotNil(t, result1)

	// Second call with the same index should be no-op.
	result2, err := execCompactLog(state, req)
	require.NoError(t, err)
	assert.Nil(t, result2, "second compact at same index should be no-op")
}

func TestExecCompactLog_ZeroTerm(t *testing.T) {
	state := &ApplyState{
		AppliedIndex:   100,
		TruncatedIndex: 10,
		TruncatedTerm:  1,
	}

	req := CompactLogRequest{CompactIndex: 50, CompactTerm: 0}

	_, err := execCompactLog(state, req)
	assert.Error(t, err, "zero term should be rejected")
}

func TestExecCompactLog_BeyondApplied(t *testing.T) {
	state := &ApplyState{
		AppliedIndex:   50,
		TruncatedIndex: 10,
		TruncatedTerm:  1,
	}

	req := CompactLogRequest{CompactIndex: 60, CompactTerm: 3}

	_, err := execCompactLog(state, req)
	assert.Error(t, err, "compact index beyond applied should be rejected")
}

func TestPeerStorageCompactTo(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPeerStorage(1, engine)

	// Populate entries 6..105 (100 entries after initial index 5).
	entries := make([]raftpb.Entry, 100)
	for i := 0; i < 100; i++ {
		entries[i] = raftpb.Entry{
			Index: RaftInitLogIndex + uint64(i) + 1,
			Term:  1,
			Data:  []byte{byte(i)},
		}
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	// Verify we can read all entries.
	got, err := s.Entries(RaftInitLogIndex+1, RaftInitLogIndex+101, 0)
	require.NoError(t, err)
	assert.Len(t, got, 100)

	// Compact to index 55 (remove cache entries with index < 55).
	s.CompactTo(55)

	// Also advance the truncated index so Entries() returns ErrCompacted for old indices.
	s.SetApplyState(ApplyState{
		AppliedIndex:   RaftInitLogIndex + 100,
		TruncatedIndex: 54,
		TruncatedTerm:  1,
	})

	// Entries before 55 should be compacted.
	_, err = s.Entries(RaftInitLogIndex+1, 55, 0)
	assert.Equal(t, raft.ErrCompacted, err)

	// Entries from 55 onward should still be available (from engine fallback).
	got, err = s.Entries(55, RaftInitLogIndex+101, 0)
	require.NoError(t, err)
	assert.True(t, len(got) > 0)
}

func TestPeerStorageTruncatedAccessors(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPeerStorage(1, engine)

	assert.Equal(t, RaftInitLogIndex, s.TruncatedIndex())
	assert.Equal(t, RaftInitLogTerm, s.TruncatedTerm())

	s.SetApplyState(ApplyState{
		AppliedIndex:   100,
		TruncatedIndex: 50,
		TruncatedTerm:  7,
	})

	assert.Equal(t, uint64(50), s.TruncatedIndex())
	assert.Equal(t, uint64(7), s.TruncatedTerm())
	assert.Equal(t, uint64(100), s.AppliedIndex())
}

func TestRaftLogGCWorker(t *testing.T) {
	engine := newTestEngine(t)

	// Write some raft log entries to the engine.
	regionID := uint64(1)
	for i := uint64(1); i <= 10; i++ {
		entry := raftpb.Entry{Index: i, Term: 1, Data: []byte{byte(i)}}
		data, err := entry.Marshal()
		require.NoError(t, err)
		require.NoError(t, engine.Put(cfnames.CFRaft, keys.RaftLogKey(regionID, i), data))
	}

	// Verify entries are there.
	for i := uint64(1); i <= 10; i++ {
		_, err := engine.Get(cfnames.CFRaft, keys.RaftLogKey(regionID, i))
		require.NoError(t, err)
	}

	taskCh := make(chan RaftLogGCTask, 10)
	stopCh := make(chan struct{})
	worker := NewRaftLogGCWorker(engine, taskCh, stopCh)

	go worker.Run()

	// Send a task to delete entries [1, 6) => entries 1-5.
	taskCh <- RaftLogGCTask{
		RegionID: regionID,
		StartIdx: 1,
		EndIdx:   6,
	}

	// Give the worker time to process.
	time.Sleep(100 * time.Millisecond)

	close(stopCh)

	// Entries 1-5 should be deleted.
	for i := uint64(1); i <= 5; i++ {
		_, err := engine.Get(cfnames.CFRaft, keys.RaftLogKey(regionID, i))
		assert.Error(t, err, "entry %d should be deleted", i)
	}

	// Entries 6-10 should still exist.
	for i := uint64(6); i <= 10; i++ {
		_, err := engine.Get(cfnames.CFRaft, keys.RaftLogKey(regionID, i))
		require.NoError(t, err, "entry %d should still exist", i)
	}
}

func TestMarshalUnmarshalCompactLogRequest(t *testing.T) {
	req := CompactLogRequest{
		CompactIndex: 12345,
		CompactTerm:  67,
	}

	data := marshalCompactLogRequest(req)
	got, ok := unmarshalCompactLogRequest(data)
	require.True(t, ok)
	assert.Equal(t, req.CompactIndex, got.CompactIndex)
	assert.Equal(t, req.CompactTerm, got.CompactTerm)
}

func TestUnmarshalCompactLogRequest_Invalid(t *testing.T) {
	// Too short.
	_, ok := unmarshalCompactLogRequest([]byte{0x01})
	assert.False(t, ok)

	// Wrong tag.
	data := make([]byte, 17)
	data[0] = 0xFF
	_, ok = unmarshalCompactLogRequest(data)
	assert.False(t, ok)
}

func TestOnReadyCompactLog(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPeerStorage(1, engine)

	// Populate entries.
	entries := make([]raftpb.Entry, 50)
	for i := 0; i < 50; i++ {
		entries[i] = raftpb.Entry{
			Index: RaftInitLogIndex + uint64(i) + 1,
			Term:  1,
			Data:  []byte{byte(i)},
		}
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	s.SetApplyState(ApplyState{
		AppliedIndex:   RaftInitLogIndex + 50,
		TruncatedIndex: RaftInitLogIndex,
		TruncatedTerm:  RaftInitLogTerm,
	})

	taskCh := make(chan RaftLogGCTask, 10)

	// Create a minimal peer-like structure to test onReadyCompactLog.
	p := &Peer{
		regionID:      1,
		storage:       s,
		raftLogSizeHint: 1000,
		logGCWorkerCh: taskCh,
	}

	result := CompactLogResult{
		TruncatedIndex: 30,
		TruncatedTerm:  1,
		FirstIndex:     RaftInitLogIndex + 1,
	}

	p.onReadyCompactLog(result)

	// Check that raftLogSizeHint was reduced.
	assert.True(t, p.raftLogSizeHint < 1000, "raftLogSizeHint should be reduced")

	// Check that lastCompactedIdx was updated.
	assert.Equal(t, uint64(30), p.lastCompactedIdx)

	// Check that a GC task was sent.
	select {
	case task := <-taskCh:
		assert.Equal(t, uint64(1), task.RegionID)
		assert.Equal(t, uint64(31), task.EndIdx) // TruncatedIndex + 1
	default:
		t.Fatal("expected a GC task to be sent")
	}
}
