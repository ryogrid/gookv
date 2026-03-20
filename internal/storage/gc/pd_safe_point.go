package gc

import (
	"context"

	"github.com/ryogrid/gookv/pkg/pdclient"
	"github.com/ryogrid/gookv/pkg/txntypes"
)

// PDSafePointProvider retrieves the GC safe point from PD.
type PDSafePointProvider struct {
	pdClient pdclient.Client
}

// NewPDSafePointProvider creates a new PDSafePointProvider.
func NewPDSafePointProvider(pdClient pdclient.Client) *PDSafePointProvider {
	return &PDSafePointProvider{pdClient: pdClient}
}

// GetGCSafePoint returns the cluster-wide GC safe point from PD.
func (p *PDSafePointProvider) GetGCSafePoint(ctx context.Context) (txntypes.TimeStamp, error) {
	sp, err := p.pdClient.GetGCSafePoint(ctx)
	if err != nil {
		return 0, err
	}
	return txntypes.TimeStamp(sp), nil
}
