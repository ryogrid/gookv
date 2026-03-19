package server

import (
	"fmt"
	"sync"

	"github.com/ryogrid/gookv/internal/server/transport"
)

// Ensure StaticStoreResolver implements transport.StoreResolver.
var _ transport.StoreResolver = (*StaticStoreResolver)(nil)

// StaticStoreResolver resolves store IDs to network addresses using a static map.
// Used for cluster setup without PD.
type StaticStoreResolver struct {
	mu    sync.RWMutex
	addrs map[uint64]string // storeID -> address
}

// NewStaticStoreResolver creates a resolver with the given storeID → address mapping.
func NewStaticStoreResolver(addrs map[uint64]string) *StaticStoreResolver {
	copied := make(map[uint64]string, len(addrs))
	for k, v := range addrs {
		copied[k] = v
	}
	return &StaticStoreResolver{addrs: copied}
}

// ResolveStore returns the address for the given store ID.
func (r *StaticStoreResolver) ResolveStore(storeID uint64) (string, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	addr, ok := r.addrs[storeID]
	if !ok {
		return "", fmt.Errorf("unknown store %d", storeID)
	}
	return addr, nil
}

// UpdateAddr updates the address for a store ID.
func (r *StaticStoreResolver) UpdateAddr(storeID uint64, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.addrs[storeID] = addr
}
