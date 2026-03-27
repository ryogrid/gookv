package e2e_external_test

import (
	"context"
	"testing"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/pkg/e2elib"
)

// TestRegionSplitWithPD tests the end-to-end region split flow via PD APIs.
// This test exercises the PD split ID allocation and reporting path.
func TestRegionSplitWithPD(t *testing.T) {
	e2elib.SkipIfNoBinary(t, "gookv-pd")

	alloc := e2elib.NewPortAllocator()
	t.Cleanup(func() { alloc.ReleaseAll() })

	pd := e2elib.NewPDNode(t, alloc, e2elib.PDNodeConfig{})
	require.NoError(t, pd.Start())
	require.NoError(t, pd.WaitForReady(15*time.Second))

	pdClient := pd.Client()
	ctx := context.Background()

	// Bootstrap cluster.
	store := &metapb.Store{Id: 1, Address: "127.0.0.1:20160"}
	region := &metapb.Region{
		Id:       1,
		StartKey: nil,
		EndKey:   nil,
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 1,
		},
		Peers: []*metapb.Peer{
			{Id: 1, StoreId: 1},
		},
	}
	_, err := pdClient.Bootstrap(ctx, store, region)
	require.NoError(t, err)

	// Request split IDs from PD.
	splitResp, err := pdClient.AskBatchSplit(ctx, region, 1)
	require.NoError(t, err)
	require.Len(t, splitResp.GetIds(), 1, "should get 1 split ID set")

	splitID := splitResp.GetIds()[0]
	newRegionID := splitID.GetNewRegionId()
	assert.NotZero(t, newRegionID, "new region ID should be non-zero")
	require.NotEmpty(t, splitID.GetNewPeerIds(), "should have new peer IDs")

	// Build peers for the new region from the allocated peer IDs.
	var newPeers []*metapb.Peer
	for i, pid := range splitID.GetNewPeerIds() {
		storeID := uint64(1)
		if i < len(region.GetPeers()) {
			storeID = region.GetPeers()[i].GetStoreId()
		}
		newPeers = append(newPeers, &metapb.Peer{Id: pid, StoreId: storeID})
	}

	// Simulate split: create left and right regions.
	splitKey := []byte("m")
	leftRegion := &metapb.Region{
		Id:       region.GetId(),
		StartKey: nil,
		EndKey:   splitKey,
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 2, // bumped
		},
		Peers: region.GetPeers(),
	}
	rightRegion := &metapb.Region{
		Id:       newRegionID,
		StartKey: splitKey,
		EndKey:   nil,
		RegionEpoch: &metapb.RegionEpoch{
			ConfVer: 1,
			Version: 2,
		},
		Peers: newPeers,
	}

	// Report split to PD.
	err = pdClient.ReportBatchSplit(ctx, []*metapb.Region{leftRegion, rightRegion})
	require.NoError(t, err)

	// Verify PD metadata: key before split point should be in left region.
	e2elib.WaitForCondition(t, 10*time.Second, "PD reflects split", func() bool {
		r, _, err := pdClient.GetRegion(ctx, []byte("a"))
		return err == nil && r != nil && string(r.GetEndKey()) == string(splitKey)
	})

	gotLeft, _, err := pdClient.GetRegion(ctx, []byte("a"))
	require.NoError(t, err)
	assert.Equal(t, region.GetId(), gotLeft.GetId(), "left region should keep original ID")
	assert.Equal(t, splitKey, gotLeft.GetEndKey())

	// Key after split point should be in right region.
	gotRight, _, err := pdClient.GetRegion(ctx, []byte("z"))
	require.NoError(t, err)
	assert.Equal(t, newRegionID, gotRight.GetId(), "right region should have new ID")
	assert.Equal(t, splitKey, gotRight.GetStartKey())

	t.Log("Region split with PD passed")
}
