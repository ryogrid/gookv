package pd

import (
	"math"
	"path/filepath"
	"testing"

	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"

	"github.com/ryogrid/gookv/internal/engine/rocks"
	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/internal/raftstore"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestEngine(t *testing.T) traits.KvEngine {
	t.Helper()
	dir := t.TempDir()
	e, err := rocks.Open(filepath.Join(dir, "test-db"))
	require.NoError(t, err)
	t.Cleanup(func() { e.Close() })
	return e
}

// TestPDRaftStorage_InitialState verifies that a fresh storage returns
// empty hard state and empty conf state.
func TestPDRaftStorage_InitialState(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	hs, cs, err := s.InitialState()
	require.NoError(t, err)
	assert.Equal(t, raftpb.HardState{}, hs)
	assert.Equal(t, raftpb.ConfState{}, cs)
}

// TestPDRaftStorage_SaveReady verifies that SaveReady persists entries and
// hard state, and that they can be read back via LastIndex, Term, and Entries.
func TestPDRaftStorage_SaveReady(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	entries := []raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("entry-1")},
		{Index: 2, Term: 1, Data: []byte("entry-2")},
		{Index: 3, Term: 1, Data: []byte("entry-3")},
	}
	hs := raftpb.HardState{Term: 1, Vote: 1, Commit: 3}
	rd := raft.Ready{
		HardState: hs,
		Entries:   entries,
	}

	require.NoError(t, s.SaveReady(rd))

	// Verify last index.
	last, err := s.LastIndex()
	require.NoError(t, err)
	assert.Equal(t, uint64(3), last)

	// Verify term.
	term, err := s.Term(2)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), term)

	// Verify entries.
	gotEntries, err := s.Entries(1, 4, math.MaxUint64)
	require.NoError(t, err)
	assert.Len(t, gotEntries, 3)
	assert.Equal(t, []byte("entry-1"), gotEntries[0].Data)
	assert.Equal(t, []byte("entry-3"), gotEntries[2].Data)

	// Verify hard state.
	gotHS, _, err := s.InitialState()
	require.NoError(t, err)
	assert.Equal(t, hs, gotHS)
}

// TestPDRaftStorage_RecoverFromEngine verifies the round-trip: persist entries
// via SaveReady, create a new storage on the same engine, recover, and verify.
func TestPDRaftStorage_RecoverFromEngine(t *testing.T) {
	engine := newTestEngine(t)
	s1 := NewPDRaftStorage(1, engine)

	// Save 5 entries.
	entries := make([]raftpb.Entry, 5)
	for i := range entries {
		entries[i] = raftpb.Entry{
			Index: uint64(i + 1),
			Term:  1,
			Data:  []byte("data"),
		}
	}
	hs := raftpb.HardState{Term: 1, Vote: 1, Commit: 5}
	rd := raft.Ready{
		HardState: hs,
		Entries:   entries,
	}
	require.NoError(t, s1.SaveReady(rd))

	// Create a new storage on the same engine and recover.
	s2 := NewPDRaftStorage(1, engine)
	require.NoError(t, s2.RecoverFromEngine())

	// Verify last index.
	last, err := s2.LastIndex()
	require.NoError(t, err)
	assert.Equal(t, uint64(5), last)

	// Verify all entries can be read back.
	gotEntries, err := s2.Entries(1, 6, math.MaxUint64)
	require.NoError(t, err)
	assert.Len(t, gotEntries, 5)

	// Verify hard state was recovered.
	gotHS, _, err := s2.InitialState()
	require.NoError(t, err)
	assert.Equal(t, hs, gotHS)
}

// TestPDRaftStorage_Entries verifies entry reads from both cache and engine.
// After compacting the cache, older entries should be read from the engine.
func TestPDRaftStorage_Entries(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	// Save 10 entries.
	entries := make([]raftpb.Entry, 10)
	for i := range entries {
		entries[i] = raftpb.Entry{
			Index: uint64(i + 1),
			Term:  1,
			Data:  []byte("data"),
		}
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	// Compact cache to index 5 (keep only 6-10 in cache).
	s.CompactTo(6)

	// Reading indices 1-5 should fall back to engine.
	got, err := s.Entries(1, 6, math.MaxUint64)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	assert.Equal(t, uint64(1), got[0].Index)
	assert.Equal(t, uint64(5), got[4].Index)

	// Reading indices 6-10 should come from cache.
	got, err = s.Entries(6, 11, math.MaxUint64)
	require.NoError(t, err)
	assert.Len(t, got, 5)
	assert.Equal(t, uint64(6), got[0].Index)
	assert.Equal(t, uint64(10), got[4].Index)
}

// TestPDRaftStorage_EntriesSizeLimit verifies that Entries respects the
// maxSize parameter by returning fewer entries when the limit is hit.
func TestPDRaftStorage_EntriesSizeLimit(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	// Save 10 entries, each ~100 bytes of data.
	entries := make([]raftpb.Entry, 10)
	for i := range entries {
		entries[i] = raftpb.Entry{
			Index: uint64(i + 1),
			Term:  1,
			Data:  make([]byte, 100),
		}
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	// Request with a size limit of 250 bytes.
	// Each entry is larger than ~100 bytes due to protobuf overhead.
	// With 250 bytes limit, we should get at most 2-3 entries.
	got, err := s.Entries(1, 11, 250)
	require.NoError(t, err)
	assert.True(t, len(got) <= 3, "expected <= 3 entries, got %d", len(got))
	assert.True(t, len(got) >= 1, "expected >= 1 entry, got %d", len(got))
}

// TestPDRaftStorage_Term verifies that Term returns the correct term
// for entries at various indices.
func TestPDRaftStorage_Term(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	// Save entries at indices 1-5 with terms [1,1,2,2,3].
	entries := []raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 2, Data: []byte("c")},
		{Index: 4, Term: 2, Data: []byte("d")},
		{Index: 5, Term: 3, Data: []byte("e")},
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	term, err := s.Term(1)
	require.NoError(t, err)
	assert.Equal(t, uint64(1), term)

	term, err = s.Term(3)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), term)

	term, err = s.Term(5)
	require.NoError(t, err)
	assert.Equal(t, uint64(3), term)
}

// TestPDRaftStorage_TermCompacted verifies that querying a term below the
// truncated index returns ErrCompacted, and at the truncated index returns
// the truncated term.
func TestPDRaftStorage_TermCompacted(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	// Save entries at indices 1-5.
	entries := []raftpb.Entry{
		{Index: 1, Term: 1, Data: []byte("a")},
		{Index: 2, Term: 1, Data: []byte("b")},
		{Index: 3, Term: 2, Data: []byte("c")},
		{Index: 4, Term: 2, Data: []byte("d")},
		{Index: 5, Term: 3, Data: []byte("e")},
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	// Set truncated index = 3, truncated term = 2.
	s.SetApplyState(raftstore.ApplyState{
		AppliedIndex:   5,
		TruncatedIndex: 3,
		TruncatedTerm:  2,
	})

	// Term(2) should return ErrCompacted (below truncated index).
	_, err := s.Term(2)
	assert.Equal(t, raft.ErrCompacted, err)

	// Term(3) should return the truncated term (at truncated index).
	term, err := s.Term(3)
	require.NoError(t, err)
	assert.Equal(t, uint64(2), term)
}

// TestPDRaftStorage_FirstLastIndex verifies FirstIndex and LastIndex tracking,
// including after setting a truncated index.
func TestPDRaftStorage_FirstLastIndex(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	// Save entries 1-5.
	entries := make([]raftpb.Entry, 5)
	for i := range entries {
		entries[i] = raftpb.Entry{
			Index: uint64(i + 1),
			Term:  1,
			Data:  []byte("data"),
		}
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	// Before truncation: first = truncatedIndex + 1 = 0 + 1 = 1.
	first, err := s.FirstIndex()
	require.NoError(t, err)
	assert.Equal(t, uint64(1), first)

	last, err := s.LastIndex()
	require.NoError(t, err)
	assert.Equal(t, uint64(5), last)

	// Set truncated index = 2.
	s.SetApplyState(raftstore.ApplyState{
		AppliedIndex:   5,
		TruncatedIndex: 2,
		TruncatedTerm:  1,
	})

	// After truncation: first = 2 + 1 = 3.
	first, err = s.FirstIndex()
	require.NoError(t, err)
	assert.Equal(t, uint64(3), first)

	// Last index is still 5.
	last, err = s.LastIndex()
	require.NoError(t, err)
	assert.Equal(t, uint64(5), last)
}

// TestPDRaftStorage_Snapshot verifies that Snapshot returns metadata with
// the current AppliedIndex.
func TestPDRaftStorage_Snapshot(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	// Save some entries.
	entries := make([]raftpb.Entry, 5)
	for i := range entries {
		entries[i] = raftpb.Entry{
			Index: uint64(i + 1),
			Term:  1,
			Data:  []byte("data"),
		}
	}
	rd := raft.Ready{Entries: entries}
	require.NoError(t, s.SaveReady(rd))

	// Set apply state.
	s.SetApplyState(raftstore.ApplyState{
		AppliedIndex:   5,
		TruncatedIndex: 2,
		TruncatedTerm:  1,
	})

	snap, err := s.Snapshot()
	require.NoError(t, err)
	assert.Equal(t, uint64(5), snap.Metadata.Index)
	assert.Equal(t, uint64(1), snap.Metadata.Term)
}

// TestPDRaftStorage_DummyEntry verifies that SetDummyEntry bootstraps
// the storage with a single entry at index 0 with term 0.
func TestPDRaftStorage_DummyEntry(t *testing.T) {
	engine := newTestEngine(t)
	s := NewPDRaftStorage(1, engine)

	s.SetDummyEntry()

	// The dummy entry should be at index 0, term 0.
	s.mu.RLock()
	require.Len(t, s.entries, 1)
	assert.Equal(t, uint64(0), s.entries[0].Index)
	assert.Equal(t, uint64(0), s.entries[0].Term)
	s.mu.RUnlock()

	// LastIndex should return 0.
	last, err := s.LastIndex()
	require.NoError(t, err)
	assert.Equal(t, uint64(0), last)
}
