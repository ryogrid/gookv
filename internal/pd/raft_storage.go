package pd

import (
	"fmt"
	"sync"

	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"

	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/internal/raftstore"
	"github.com/ryogrid/gookv/pkg/cfnames"
	"github.com/ryogrid/gookv/pkg/keys"
)

// PDRaftStorage implements raft.Storage for the PD cluster Raft group.
// It is modeled on PeerStorage in internal/raftstore/storage.go,
// using clusterID instead of regionID as the key namespace discriminator.
type PDRaftStorage struct {
	mu sync.RWMutex

	clusterID          uint64
	engine             traits.KvEngine
	hardState          raftpb.HardState
	applyState         raftstore.ApplyState
	entries            []raftpb.Entry
	persistedLastIndex uint64

	// snapGenFunc generates a snapshot of the PD server state.
	// Set by PDServer after construction to enable full snapshot support.
	snapGenFunc func() ([]byte, error)
}

// Ensure PDRaftStorage implements raft.Storage.
var _ raft.Storage = (*PDRaftStorage)(nil)

// NewPDRaftStorage creates a PDRaftStorage for the given cluster.
func NewPDRaftStorage(clusterID uint64, engine traits.KvEngine) *PDRaftStorage {
	return &PDRaftStorage{
		clusterID: clusterID,
		engine:    engine,
	}
}

// InitialState returns the initial HardState and ConfState from storage.
func (s *PDRaftStorage) InitialState() (raftpb.HardState, raftpb.ConfState, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hardState, raftpb.ConfState{}, nil
}

// Entries returns a slice of Raft log entries in [lo, hi), capped at maxSize bytes.
func (s *PDRaftStorage) Entries(lo, hi, maxSize uint64) ([]raftpb.Entry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if lo > hi {
		return nil, fmt.Errorf("pd: invalid range [%d, %d)", lo, hi)
	}

	first := s.firstIndexLocked()
	if lo < first {
		return nil, raft.ErrCompacted
	}

	last := s.lastIndexLocked()
	if hi > last+1 {
		return nil, raft.ErrUnavailable
	}

	// Try to serve from in-memory cache.
	if len(s.entries) > 0 {
		cacheFirst := s.entries[0].Index
		cacheLast := s.entries[len(s.entries)-1].Index

		if lo >= cacheFirst && hi <= cacheLast+1 {
			start := lo - cacheFirst
			end := hi - cacheFirst
			entries := s.entries[start:end]
			return limitSize(entries, maxSize), nil
		}
	}

	// Fall back to reading from engine.
	entries, err := s.readEntriesFromEngine(lo, hi)
	if err != nil {
		return nil, err
	}
	return limitSize(entries, maxSize), nil
}

// Term returns the term of the entry at the given index.
func (s *PDRaftStorage) Term(i uint64) (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if i == s.applyState.TruncatedIndex {
		return s.applyState.TruncatedTerm, nil
	}

	first := s.firstIndexLocked()
	if i < first {
		return 0, raft.ErrCompacted
	}

	last := s.lastIndexLocked()
	if i > last {
		return 0, raft.ErrUnavailable
	}

	// Try cache.
	if len(s.entries) > 0 {
		cacheFirst := s.entries[0].Index
		cacheLast := s.entries[len(s.entries)-1].Index
		if i >= cacheFirst && i <= cacheLast {
			return s.entries[i-cacheFirst].Term, nil
		}
	}

	// Fall back to engine.
	entries, err := s.readEntriesFromEngine(i, i+1)
	if err != nil {
		return 0, err
	}
	if len(entries) == 0 {
		return 0, raft.ErrUnavailable
	}
	return entries[0].Term, nil
}

// LastIndex returns the index of the last log entry.
func (s *PDRaftStorage) LastIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastIndexLocked(), nil
}

// FirstIndex returns the index of the first available log entry.
func (s *PDRaftStorage) FirstIndex() (uint64, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.firstIndexLocked(), nil
}

// Snapshot returns a snapshot of the PD state. When snapGenFunc is set,
// the snapshot includes the full serialized PD state for transfer to
// slow followers. Otherwise, only metadata is returned.
func (s *PDRaftStorage) Snapshot() (raftpb.Snapshot, error) {
	s.mu.RLock()
	genFunc := s.snapGenFunc
	applyState := s.applyState
	s.mu.RUnlock()

	// Determine the term for the snapshot index. Try to look it up from the
	// log; fall back to TruncatedTerm if the entry is already compacted.
	snapTerm := applyState.TruncatedTerm
	if applyState.AppliedIndex > 0 {
		if t, err := s.Term(applyState.AppliedIndex); err == nil {
			snapTerm = t
		}
	}

	var snapData []byte
	if genFunc != nil {
		data, err := genFunc()
		if err != nil {
			return raftpb.Snapshot{}, fmt.Errorf("pd: generate snapshot: %w", err)
		}
		snapData = data
	}

	return raftpb.Snapshot{
		Data: snapData,
		Metadata: raftpb.SnapshotMetadata{
			Index: applyState.AppliedIndex,
			Term:  snapTerm,
		},
	}, nil
}

// SetSnapshotGenFunc sets the function used to generate snapshot data.
func (s *PDRaftStorage) SetSnapshotGenFunc(f func() ([]byte, error)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.snapGenFunc = f
}

// SaveReady persists the Raft state changes from a Ready batch.
// Entries and hard state are written atomically via a WriteBatch.
func (s *PDRaftStorage) SaveReady(rd raft.Ready) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	wb := s.engine.NewWriteBatch()

	// Persist hard state.
	if !raft.IsEmptyHardState(rd.HardState) {
		s.hardState = rd.HardState
		data, err := s.hardState.Marshal()
		if err != nil {
			return fmt.Errorf("pd: marshal hard state: %w", err)
		}
		if err := wb.Put(cfnames.CFRaft, keys.RaftStateKey(s.clusterID), data); err != nil {
			return err
		}
	}

	// Persist new entries.
	for i := range rd.Entries {
		data, err := rd.Entries[i].Marshal()
		if err != nil {
			return fmt.Errorf("pd: marshal entry %d: %w", rd.Entries[i].Index, err)
		}
		if err := wb.Put(cfnames.CFRaft, keys.RaftLogKey(s.clusterID, rd.Entries[i].Index), data); err != nil {
			return err
		}
	}

	if err := wb.Commit(); err != nil {
		return fmt.Errorf("pd: commit ready: %w", err)
	}

	// Update in-memory cache and persisted index tracker.
	if len(rd.Entries) > 0 {
		s.appendToCache(rd.Entries)
		lastEntry := rd.Entries[len(rd.Entries)-1]
		if lastEntry.Index > s.persistedLastIndex {
			s.persistedLastIndex = lastEntry.Index
		}
	}

	return nil
}

// RecoverFromEngine restores PDRaftStorage state from the engine.
// This should be called when restarting a PD node that already has persisted Raft state.
func (s *PDRaftStorage) RecoverFromEngine() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Recover hard state.
	data, err := s.engine.Get(cfnames.CFRaft, keys.RaftStateKey(s.clusterID))
	if err == nil {
		var hs raftpb.HardState
		if err := hs.Unmarshal(data); err != nil {
			return fmt.Errorf("pd: unmarshal hard state: %w", err)
		}
		s.hardState = hs
	} else if err != traits.ErrNotFound {
		return fmt.Errorf("pd: read hard state: %w", err)
	}

	// Scan Raft log entries to find the last persisted index and rebuild cache.
	startKey, endKey := keys.RaftLogKeyRange(s.clusterID)
	iter := s.engine.NewIterator(cfnames.CFRaft, traits.IterOptions{
		LowerBound: startKey,
		UpperBound: endKey,
	})
	defer iter.Close()

	var lastIdx uint64
	var entries []raftpb.Entry
	for iter.SeekToFirst(); iter.Valid(); iter.Next() {
		var entry raftpb.Entry
		if err := entry.Unmarshal(iter.Value()); err != nil {
			return fmt.Errorf("pd: unmarshal entry during recovery: %w", err)
		}
		entries = append(entries, entry)
		if entry.Index > lastIdx {
			lastIdx = entry.Index
		}
	}
	if err := iter.Error(); err != nil {
		return fmt.Errorf("pd: iterate entries during recovery: %w", err)
	}

	if lastIdx > 0 {
		s.persistedLastIndex = lastIdx
		// Keep only the most recent entries in cache.
		const maxCacheSize = 1024
		if len(entries) > maxCacheSize {
			entries = entries[len(entries)-maxCacheSize:]
		}
		s.entries = entries
	}

	return nil
}

// SetApplyState updates the apply state.
func (s *PDRaftStorage) SetApplyState(state raftstore.ApplyState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyState = state
}

// GetApplyState returns the current apply state.
func (s *PDRaftStorage) GetApplyState() raftstore.ApplyState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.applyState
}

// SetDummyEntry adds a dummy entry at index 0 with term 0,
// matching etcd/raft's MemoryStorage convention for empty storage.
func (s *PDRaftStorage) SetDummyEntry() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = []raftpb.Entry{{Index: 0, Term: 0}}
}

// SetPersistedLastIndex sets the persisted last index (for initialization).
func (s *PDRaftStorage) SetPersistedLastIndex(idx uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistedLastIndex = idx
}

// AppliedIndex returns the current applied index.
func (s *PDRaftStorage) AppliedIndex() uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.applyState.AppliedIndex
}

// CompactTo removes entries from the in-memory cache up to compactTo.
// Entries before compactTo will no longer be served from cache.
func (s *PDRaftStorage) CompactTo(compactTo uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if len(s.entries) == 0 {
		return
	}

	cacheFirst := s.entries[0].Index
	if compactTo <= cacheFirst {
		return
	}

	offset := compactTo - cacheFirst
	if offset >= uint64(len(s.entries)) {
		s.entries = nil
		return
	}
	s.entries = s.entries[offset:]
}

// DeleteEntriesTo deletes persisted Raft log entries in [1, endIdx) from the engine.
// This is the physical counterpart to CompactTo (which only trims the in-memory cache).
// It uses engine.DeleteRange for efficient bulk deletion, following the same pattern
// as RaftLogGCWorker.gcRaftLog in internal/raftstore/raftlog_gc.go.
func (s *PDRaftStorage) DeleteEntriesTo(endIdx uint64) error {
	if endIdx <= 1 {
		return nil
	}
	startKey := keys.RaftLogKey(s.clusterID, 0)
	endKey := keys.RaftLogKey(s.clusterID, endIdx)
	return s.engine.DeleteRange(cfnames.CFRaft, startKey, endKey)
}

// --- Internal helpers ---

func (s *PDRaftStorage) firstIndexLocked() uint64 {
	return s.applyState.TruncatedIndex + 1
}

func (s *PDRaftStorage) lastIndexLocked() uint64 {
	if len(s.entries) > 0 {
		return s.entries[len(s.entries)-1].Index
	}
	return s.persistedLastIndex
}

func (s *PDRaftStorage) appendToCache(entries []raftpb.Entry) {
	if len(s.entries) == 0 {
		s.entries = append(s.entries, entries...)
		return
	}

	// Truncate cache if new entries overlap.
	first := entries[0].Index
	cacheFirst := s.entries[0].Index
	if first <= cacheFirst {
		s.entries = append([]raftpb.Entry{}, entries...)
	} else {
		// Keep cache entries before the new ones.
		keepTo := first - cacheFirst
		if keepTo > uint64(len(s.entries)) {
			keepTo = uint64(len(s.entries))
		}
		s.entries = append(s.entries[:keepTo], entries...)
	}

	// Limit cache size (keep last 1024 entries).
	const maxCacheSize = 1024
	if len(s.entries) > maxCacheSize {
		s.entries = s.entries[len(s.entries)-maxCacheSize:]
	}
}

func (s *PDRaftStorage) readEntriesFromEngine(lo, hi uint64) ([]raftpb.Entry, error) {
	var entries []raftpb.Entry
	for idx := lo; idx < hi; idx++ {
		data, err := s.engine.Get(cfnames.CFRaft, keys.RaftLogKey(s.clusterID, idx))
		if err != nil {
			if err == traits.ErrNotFound {
				break
			}
			return nil, fmt.Errorf("pd: read entry %d: %w", idx, err)
		}
		var entry raftpb.Entry
		if err := entry.Unmarshal(data); err != nil {
			return nil, fmt.Errorf("pd: unmarshal entry %d: %w", idx, err)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// limitSize returns a prefix of entries whose total size does not exceed maxSize.
// If maxSize is 0, all entries are returned.
func limitSize(entries []raftpb.Entry, maxSize uint64) []raftpb.Entry {
	if maxSize == 0 || len(entries) == 0 {
		return entries
	}
	var size uint64
	for i, e := range entries {
		size += uint64(e.Size())
		if size > maxSize && i > 0 {
			return entries[:i]
		}
	}
	return entries
}
