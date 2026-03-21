package pd

import (
	"strings"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPDCommand_MarshalRoundTrip(t *testing.T) {
	boolTrue := true
	storeStateUp := StoreStateUp

	tests := []struct {
		name string
		cmd  PDCommand
	}{
		{
			name: "CmdSetBootstrapped",
			cmd: PDCommand{
				Type:         CmdSetBootstrapped,
				Bootstrapped: &boolTrue,
			},
		},
		{
			name: "CmdPutStore",
			cmd: PDCommand{
				Type: CmdPutStore,
				Store: &metapb.Store{
					Id:      1,
					Address: "127.0.0.1:20160",
				},
			},
		},
		{
			name: "CmdPutRegion",
			cmd: PDCommand{
				Type: CmdPutRegion,
				Region: &metapb.Region{
					Id:       100,
					StartKey: []byte("a"),
					EndKey:   []byte("z"),
					RegionEpoch: &metapb.RegionEpoch{
						ConfVer: 1,
						Version: 1,
					},
					Peers: []*metapb.Peer{
						{Id: 101, StoreId: 1},
					},
				},
				Leader: &metapb.Peer{Id: 101, StoreId: 1},
			},
		},
		{
			name: "CmdUpdateStoreStats",
			cmd: PDCommand{
				Type: CmdUpdateStoreStats,
				StoreStats: &pdpb.StoreStats{
					StoreId:     1,
					Capacity:    1024 * 1024 * 1024,
					Available:   512 * 1024 * 1024,
					RegionCount: 10,
				},
			},
		},
		{
			name: "CmdSetStoreState",
			cmd: PDCommand{
				Type:       CmdSetStoreState,
				StoreID:    1,
				StoreState: &storeStateUp,
			},
		},
		{
			name: "CmdTSOAllocate",
			cmd: PDCommand{
				Type:         CmdTSOAllocate,
				TSOBatchSize: 1000,
			},
		},
		{
			name: "CmdIDAlloc",
			cmd: PDCommand{
				Type:        CmdIDAlloc,
				IDBatchSize: 100,
			},
		},
		{
			name: "CmdUpdateGCSafePoint",
			cmd: PDCommand{
				Type:        CmdUpdateGCSafePoint,
				GCSafePoint: 424242,
			},
		},
		{
			name: "CmdStartMove",
			cmd: PDCommand{
				Type:              CmdStartMove,
				MoveRegionID:      100,
				MoveSourcePeer:    &metapb.Peer{Id: 101, StoreId: 1},
				MoveTargetStoreID: 2,
			},
		},
		{
			name: "CmdAdvanceMove",
			cmd: PDCommand{
				Type: CmdAdvanceMove,
				AdvanceRegion: &metapb.Region{
					Id: 100,
					RegionEpoch: &metapb.RegionEpoch{
						ConfVer: 2,
						Version: 1,
					},
					Peers: []*metapb.Peer{
						{Id: 101, StoreId: 1},
						{Id: 102, StoreId: 2},
					},
				},
				AdvanceLeader: &metapb.Peer{Id: 101, StoreId: 1},
			},
		},
		{
			name: "CmdCleanupStaleMove",
			cmd: PDCommand{
				Type:           CmdCleanupStaleMove,
				CleanupTimeout: 5 * time.Minute,
			},
		},
		{
			name: "CmdCompactLog",
			cmd: PDCommand{
				Type:         CmdCompactLog,
				CompactIndex: 42,
				CompactTerm:  7,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := tt.cmd.Marshal()
			require.NoError(t, err)

			// First byte must be the type.
			assert.Equal(t, byte(tt.cmd.Type), data[0])

			got, err := UnmarshalPDCommand(data)
			require.NoError(t, err)

			assert.Equal(t, tt.cmd.Type, got.Type)

			// Compare the full struct via re-marshalling to avoid protobuf
			// internal field comparison issues.
			expectedData, err := tt.cmd.Marshal()
			require.NoError(t, err)
			gotData, err := got.Marshal()
			require.NoError(t, err)
			assert.Equal(t, expectedData, gotData)
		})
	}
}

func TestPDCommand_InvalidType(t *testing.T) {
	// Unknown type byte 0xFF.
	data := []byte{0xFF, '{', '}'}
	_, err := UnmarshalPDCommand(data)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unknown type")
}

func TestPDCommand_EmptyPayload(t *testing.T) {
	cmd := PDCommand{Type: CmdCleanupStaleMove}

	data, err := cmd.Marshal()
	require.NoError(t, err)

	got, err := UnmarshalPDCommand(data)
	require.NoError(t, err)
	assert.Equal(t, CmdCleanupStaleMove, got.Type)
	// Zero-value fields should not cause panics.
	assert.Nil(t, got.Store)
	assert.Nil(t, got.Region)
	assert.Nil(t, got.Leader)
	assert.Equal(t, uint64(0), got.GCSafePoint)
	assert.Equal(t, time.Duration(0), got.CleanupTimeout)
}

func TestPDCommand_LargePayload(t *testing.T) {
	largeAddr := strings.Repeat("x", 1000)
	cmd := PDCommand{
		Type: CmdPutStore,
		Store: &metapb.Store{
			Id:      42,
			Address: largeAddr,
		},
	}

	data, err := cmd.Marshal()
	require.NoError(t, err)

	got, err := UnmarshalPDCommand(data)
	require.NoError(t, err)
	require.NotNil(t, got.Store)
	assert.Equal(t, largeAddr, got.Store.Address)
	assert.Equal(t, uint64(42), got.Store.Id)
}

func TestPDCommand_AllFieldsPreserved(t *testing.T) {
	cmd := PDCommand{
		Type: CmdPutRegion,
		Region: &metapb.Region{
			Id:       200,
			StartKey: []byte("aaa"),
			EndKey:   []byte("zzz"),
			RegionEpoch: &metapb.RegionEpoch{
				ConfVer: 3,
				Version: 5,
			},
			Peers: []*metapb.Peer{
				{Id: 201, StoreId: 1},
				{Id: 202, StoreId: 2},
				{Id: 203, StoreId: 3},
			},
		},
		Leader: &metapb.Peer{Id: 201, StoreId: 1},
	}

	data, err := cmd.Marshal()
	require.NoError(t, err)

	got, err := UnmarshalPDCommand(data)
	require.NoError(t, err)

	require.NotNil(t, got.Region)
	assert.Len(t, got.Region.Peers, 3)
	assert.Equal(t, uint64(200), got.Region.Id)
	assert.Equal(t, uint64(3), got.Region.RegionEpoch.ConfVer)
	assert.Equal(t, uint64(5), got.Region.RegionEpoch.Version)
	assert.Equal(t, []byte("aaa"), got.Region.StartKey)
	assert.Equal(t, []byte("zzz"), got.Region.EndKey)

	require.NotNil(t, got.Leader)
	assert.Equal(t, uint64(201), got.Leader.Id)
	assert.Equal(t, uint64(1), got.Leader.StoreId)
}
