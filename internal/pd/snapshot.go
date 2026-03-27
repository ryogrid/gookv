package pd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
)

// TSOSnapshotState captures the TSOAllocator state for snapshotting.
type TSOSnapshotState struct {
	Physical int64 `json:"physical"`
	Logical  int64 `json:"logical"`
}

// PDSnapshot captures all mutable PD state for Raft snapshot transfer.
type PDSnapshot struct {
	Bootstrapped bool                        `json:"bootstrapped"`
	Stores       map[uint64]*metapb.Store    `json:"stores"`
	Regions      map[uint64]*metapb.Region   `json:"regions"`
	Leaders      map[uint64]*metapb.Peer     `json:"leaders"`
	StoreStats   map[uint64]*pdpb.StoreStats `json:"store_stats"`
	StoreStates  map[uint64]StoreState       `json:"store_states"`
	NextID       uint64                      `json:"next_id"`
	TSOState     TSOSnapshotState            `json:"tso_state"`
	GCSafePoint  uint64                      `json:"gc_safe_point"`
	PendingMoves       map[uint64]*PendingMove `json:"pending_moves"`
	StoreLastHeartbeat map[uint64]int64        `json:"store_last_heartbeat"`
}

// GenerateSnapshot captures the current PD server state as a JSON-encoded snapshot.
// It acquires read locks on all mutable sub-components and deep-copies their state.
func (s *PDServer) GenerateSnapshot() ([]byte, error) {
	snap := PDSnapshot{
		Stores:       make(map[uint64]*metapb.Store),
		Regions:      make(map[uint64]*metapb.Region),
		Leaders:      make(map[uint64]*metapb.Peer),
		StoreStats:   make(map[uint64]*pdpb.StoreStats),
		StoreStates:  make(map[uint64]StoreState),
		PendingMoves: make(map[uint64]*PendingMove),
	}

	// Snapshot MetadataStore (read lock).
	s.meta.mu.RLock()
	snap.Bootstrapped = s.meta.bootstrapped
	for k, v := range s.meta.stores {
		snap.Stores[k] = v
	}
	for k, v := range s.meta.regions {
		snap.Regions[k] = v
	}
	for k, v := range s.meta.leaders {
		snap.Leaders[k] = v
	}
	for k, v := range s.meta.storeStats {
		snap.StoreStats[k] = v
	}
	for k, v := range s.meta.storeStates {
		snap.StoreStates[k] = v
	}
	snap.StoreLastHeartbeat = make(map[uint64]int64, len(s.meta.storeLastHeartbeat))
	for k, v := range s.meta.storeLastHeartbeat {
		snap.StoreLastHeartbeat[k] = v.UnixNano()
	}
	s.meta.mu.RUnlock()

	// Snapshot TSOAllocator.
	s.tso.mu.Lock()
	snap.TSOState.Physical = s.tso.physical
	snap.TSOState.Logical = s.tso.logical
	s.tso.mu.Unlock()

	// Snapshot IDAllocator.
	s.idAlloc.mu.Lock()
	snap.NextID = s.idAlloc.nextID
	s.idAlloc.mu.Unlock()

	// Snapshot GCSafePointManager.
	s.gcMgr.mu.Lock()
	snap.GCSafePoint = s.gcMgr.safePoint
	s.gcMgr.mu.Unlock()

	// Snapshot MoveTracker.
	s.moveTracker.mu.Lock()
	for k, v := range s.moveTracker.moves {
		snap.PendingMoves[k] = v
	}
	s.moveTracker.mu.Unlock()

	data, err := json.Marshal(&snap)
	if err != nil {
		return nil, fmt.Errorf("pd: marshal snapshot: %w", err)
	}
	return data, nil
}

// ApplySnapshot replaces the PD server's in-memory state with the state from
// a JSON-encoded snapshot. It acquires write locks on all mutable sub-components.
func (s *PDServer) ApplySnapshot(data []byte) error {
	var snap PDSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return fmt.Errorf("pd: unmarshal snapshot: %w", err)
	}

	// Apply to MetadataStore (write lock).
	s.meta.mu.Lock()
	s.meta.bootstrapped = snap.Bootstrapped
	s.meta.stores = snap.Stores
	if s.meta.stores == nil {
		s.meta.stores = make(map[uint64]*metapb.Store)
	}
	s.meta.regions = snap.Regions
	if s.meta.regions == nil {
		s.meta.regions = make(map[uint64]*metapb.Region)
	}
	s.meta.leaders = snap.Leaders
	if s.meta.leaders == nil {
		s.meta.leaders = make(map[uint64]*metapb.Peer)
	}
	s.meta.storeStats = snap.StoreStats
	if s.meta.storeStats == nil {
		s.meta.storeStats = make(map[uint64]*pdpb.StoreStats)
	}
	s.meta.storeStates = snap.StoreStates
	if s.meta.storeStates == nil {
		s.meta.storeStates = make(map[uint64]StoreState)
	}
	s.meta.storeLastHeartbeat = make(map[uint64]time.Time, len(snap.StoreLastHeartbeat))
	for k, v := range snap.StoreLastHeartbeat {
		s.meta.storeLastHeartbeat[k] = time.Unix(0, v)
	}
	s.meta.mu.Unlock()

	// Apply to TSOAllocator.
	s.tso.mu.Lock()
	s.tso.physical = snap.TSOState.Physical
	s.tso.logical = snap.TSOState.Logical
	s.tso.mu.Unlock()

	// Apply to IDAllocator.
	s.idAlloc.mu.Lock()
	s.idAlloc.nextID = snap.NextID
	s.idAlloc.mu.Unlock()

	// Apply to GCSafePointManager.
	s.gcMgr.mu.Lock()
	s.gcMgr.safePoint = snap.GCSafePoint
	s.gcMgr.mu.Unlock()

	// Apply to MoveTracker.
	s.moveTracker.mu.Lock()
	s.moveTracker.moves = snap.PendingMoves
	if s.moveTracker.moves == nil {
		s.moveTracker.moves = make(map[uint64]*PendingMove)
	}
	s.moveTracker.mu.Unlock()

	return nil
}
