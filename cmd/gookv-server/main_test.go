package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBootstrapStoreIDs(t *testing.T) {
	tests := []struct {
		name          string
		clusterMap    map[uint64]string
		maxPeerCount  int
		wantBootstrap []uint64
		wantJoin      []uint64
	}{
		{
			name:          "3 nodes, maxPeer=3 (all bootstrap)",
			clusterMap:    map[uint64]string{1: "a", 2: "b", 3: "c"},
			maxPeerCount:  3,
			wantBootstrap: []uint64{1, 2, 3},
			wantJoin:      []uint64{},
		},
		{
			name:          "5 nodes, maxPeer=3 (3 bootstrap, 2 join)",
			clusterMap:    map[uint64]string{1: "a", 2: "b", 3: "c", 4: "d", 5: "e"},
			maxPeerCount:  3,
			wantBootstrap: []uint64{1, 2, 3},
			wantJoin:      []uint64{4, 5},
		},
		{
			name:          "7 nodes, maxPeer=3 (3 bootstrap, 4 join)",
			clusterMap:    map[uint64]string{1: "a", 2: "b", 3: "c", 4: "d", 5: "e", 6: "f", 7: "g"},
			maxPeerCount:  3,
			wantBootstrap: []uint64{1, 2, 3},
			wantJoin:      []uint64{4, 5, 6, 7},
		},
		{
			name:          "3 nodes, maxPeer=5 (all bootstrap, maxPeer exceeds nodes)",
			clusterMap:    map[uint64]string{1: "a", 2: "b", 3: "c"},
			maxPeerCount:  5,
			wantBootstrap: []uint64{1, 2, 3},
			wantJoin:      []uint64{},
		},
		{
			name:          "1 node, maxPeer=3",
			clusterMap:    map[uint64]string{1: "a"},
			maxPeerCount:  3,
			wantBootstrap: []uint64{1},
			wantJoin:      []uint64{},
		},
		{
			name:          "5 nodes, maxPeer=0 (no limit, all bootstrap)",
			clusterMap:    map[uint64]string{1: "a", 2: "b", 3: "c", 4: "d", 5: "e"},
			maxPeerCount:  0,
			wantBootstrap: []uint64{1, 2, 3, 4, 5},
			wantJoin:      []uint64{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bootstrap, join := bootstrapStoreIDs(tt.clusterMap, tt.maxPeerCount)
			assert.Equal(t, tt.wantBootstrap, bootstrap, "bootstrap IDs")
			assert.Equal(t, tt.wantJoin, join, "join IDs")

			// Verify union = all store IDs
			all := make(map[uint64]bool)
			for _, id := range bootstrap {
				all[id] = true
			}
			for _, id := range join {
				all[id] = true
			}
			assert.Equal(t, len(tt.clusterMap), len(all), "union should cover all stores")
		})
	}
}
