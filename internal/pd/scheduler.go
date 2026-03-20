package pd

import (
	"github.com/pingcap/kvproto/pkg/eraftpb"
	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
)

// Scheduler produces scheduling commands based on cluster state.
type Scheduler struct {
	meta         *MetadataStore
	idAlloc      *IDAllocator
	maxPeerCount int
}

// NewScheduler creates a new Scheduler.
func NewScheduler(meta *MetadataStore, idAlloc *IDAllocator, maxPeerCount int) *Scheduler {
	if maxPeerCount <= 0 {
		maxPeerCount = 3
	}
	return &Scheduler{
		meta:         meta,
		idAlloc:      idAlloc,
		maxPeerCount: maxPeerCount,
	}
}

// ScheduleCommand represents a scheduling command to return in a heartbeat response.
type ScheduleCommand struct {
	RegionID       uint64
	TransferLeader *pdpb.TransferLeader
	ChangePeer     *pdpb.ChangePeer
	Merge          *pdpb.Merge
}

// Schedule evaluates the cluster state for a given region and returns a command if needed.
// Returns nil if no scheduling action is required.
func (s *Scheduler) Schedule(regionID uint64, region *metapb.Region, leader *metapb.Peer) *ScheduleCommand {
	// Try each scheduler in priority order.
	if cmd := s.scheduleReplicaRepair(regionID, region); cmd != nil {
		return cmd
	}
	if cmd := s.scheduleLeaderBalance(regionID, region, leader); cmd != nil {
		return cmd
	}
	return nil
}

// scheduleReplicaRepair checks if a region needs a new replica because one is on a dead store.
func (s *Scheduler) scheduleReplicaRepair(regionID uint64, region *metapb.Region) *ScheduleCommand {
	var alivePeers []*metapb.Peer
	for _, peer := range region.GetPeers() {
		if s.meta.IsStoreAlive(peer.GetStoreId()) {
			alivePeers = append(alivePeers, peer)
		}
	}

	if len(alivePeers) >= s.maxPeerCount {
		return nil // Enough replicas
	}

	// Find a store that doesn't already host this region.
	targetStore := s.pickStoreForRegion(region)
	if targetStore == 0 {
		return nil
	}

	newPeerID := s.idAlloc.Alloc()
	return &ScheduleCommand{
		RegionID: regionID,
		ChangePeer: &pdpb.ChangePeer{
			Peer: &metapb.Peer{
				Id:      newPeerID,
				StoreId: targetStore,
			},
			ChangeType: eraftpb.ConfChangeType_AddNode,
		},
	}
}

// scheduleLeaderBalance checks if leaders should be rebalanced across stores.
func (s *Scheduler) scheduleLeaderBalance(regionID uint64, region *metapb.Region, leader *metapb.Peer) *ScheduleCommand {
	if leader == nil {
		return nil
	}

	leaderCounts := s.meta.GetLeaderCountPerStore()
	leaderStore := leader.GetStoreId()
	leaderCount := leaderCounts[leaderStore]

	// Find store with minimum leaders among this region's peers.
	var minStore uint64
	minCount := int(^uint(0) >> 1) // max int
	for _, peer := range region.GetPeers() {
		storeID := peer.GetStoreId()
		if !s.meta.IsStoreAlive(storeID) {
			continue
		}
		if count := leaderCounts[storeID]; count < minCount {
			minCount = count
			minStore = storeID
		}
	}

	// Transfer if difference > 1.
	if minStore != 0 && minStore != leaderStore && leaderCount-minCount > 1 {
		return &ScheduleCommand{
			RegionID: regionID,
			TransferLeader: &pdpb.TransferLeader{
				Peer: &metapb.Peer{StoreId: minStore},
			},
		}
	}
	return nil
}

// pickStoreForRegion finds a live store that doesn't already host this region.
func (s *Scheduler) pickStoreForRegion(region *metapb.Region) uint64 {
	existingStores := make(map[uint64]bool)
	for _, peer := range region.GetPeers() {
		existingStores[peer.GetStoreId()] = true
	}

	stores := s.meta.GetAllStores()
	for _, store := range stores {
		storeID := store.GetId()
		if existingStores[storeID] {
			continue
		}
		if !s.meta.IsStoreAlive(storeID) {
			continue
		}
		return storeID
	}
	return 0
}
