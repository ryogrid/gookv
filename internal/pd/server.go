// Package pd implements a simplified Placement Driver server for gookv.
// It provides TSO allocation, cluster metadata management, heartbeat processing,
// ID allocation, and GC safe point management.
package pd

import (
	"context"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"google.golang.org/grpc"
)

// PDServerConfig holds configuration for the PD server.
type PDServerConfig struct {
	ListenAddr string
	DataDir    string
	ClusterID  uint64

	TSOSaveInterval           time.Duration
	TSOUpdatePhysicalInterval time.Duration

	MaxPeerCount int
}

// DefaultPDServerConfig returns default PD server configuration.
func DefaultPDServerConfig() PDServerConfig {
	return PDServerConfig{
		ListenAddr:                "0.0.0.0:2379",
		DataDir:                   "/tmp/gookv-pd",
		ClusterID:                 1,
		TSOSaveInterval:           3 * time.Second,
		TSOUpdatePhysicalInterval: 50 * time.Millisecond,
		MaxPeerCount:              3,
	}
}

// PDServer implements the pdpb.PDServer gRPC interface.
type PDServer struct {
	pdpb.UnimplementedPDServer

	cfg       PDServerConfig
	clusterID uint64

	tso     *TSOAllocator
	meta    *MetadataStore
	idAlloc *IDAllocator
	gcMgr   *GCSafePointManager

	grpcServer *grpc.Server
	listener   net.Listener

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// NewPDServer creates a new PD server.
func NewPDServer(cfg PDServerConfig) (*PDServer, error) {
	ctx, cancel := context.WithCancel(context.Background())

	meta := NewMetadataStore(cfg.ClusterID)
	tso := NewTSOAllocator(cfg.TSOSaveInterval)
	idAlloc := NewIDAllocator()
	gcMgr := NewGCSafePointManager()

	grpcSrv := grpc.NewServer()

	s := &PDServer{
		cfg:        cfg,
		clusterID:  cfg.ClusterID,
		tso:        tso,
		meta:       meta,
		idAlloc:    idAlloc,
		gcMgr:      gcMgr,
		grpcServer: grpcSrv,
		ctx:        ctx,
		cancel:     cancel,
	}

	pdpb.RegisterPDServer(grpcSrv, s)

	return s, nil
}

// Start starts the PD server.
func (s *PDServer) Start() error {
	lis, err := net.Listen("tcp", s.cfg.ListenAddr)
	if err != nil {
		return fmt.Errorf("pd: listen %s: %w", s.cfg.ListenAddr, err)
	}
	s.listener = lis

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		if err := s.grpcServer.Serve(lis); err != nil {
			select {
			case <-s.ctx.Done():
			default:
				fmt.Printf("PD gRPC server error: %v\n", err)
			}
		}
	}()

	return nil
}

// Addr returns the listen address.
func (s *PDServer) Addr() string {
	if s.listener != nil {
		return s.listener.Addr().String()
	}
	return s.cfg.ListenAddr
}

// Stop gracefully stops the PD server.
func (s *PDServer) Stop() {
	s.cancel()
	s.grpcServer.GracefulStop()
	s.wg.Wait()
}

// --- gRPC handlers ---

func (s *PDServer) GetMembers(ctx context.Context, req *pdpb.GetMembersRequest) (*pdpb.GetMembersResponse, error) {
	return &pdpb.GetMembersResponse{
		Header: s.header(),
		Leader: &pdpb.Member{
			Name:       "gookv-pd-1",
			ClientUrls: []string{"http://" + s.Addr()},
		},
	}, nil
}

func (s *PDServer) Tso(stream pdpb.PD_TsoServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		count := req.GetCount()
		if count == 0 {
			count = 1
		}

		ts, err := s.tso.Allocate(int(count))
		if err != nil {
			return err
		}

		resp := &pdpb.TsoResponse{
			Header:    s.header(),
			Count:     count,
			Timestamp: ts,
		}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *PDServer) Bootstrap(ctx context.Context, req *pdpb.BootstrapRequest) (*pdpb.BootstrapResponse, error) {
	resp := &pdpb.BootstrapResponse{Header: s.header()}

	if s.meta.IsBootstrapped() {
		resp.Header = s.errorHeader("cluster already bootstrapped")
		return resp, nil
	}

	store := req.GetStore()
	region := req.GetRegion()

	if store != nil {
		s.meta.PutStore(store)
	}
	if region != nil {
		s.meta.PutRegion(region, nil)
	}
	s.meta.SetBootstrapped(true)

	return resp, nil
}

func (s *PDServer) IsBootstrapped(ctx context.Context, req *pdpb.IsBootstrappedRequest) (*pdpb.IsBootstrappedResponse, error) {
	return &pdpb.IsBootstrappedResponse{
		Header:       s.header(),
		Bootstrapped: s.meta.IsBootstrapped(),
	}, nil
}

func (s *PDServer) AllocID(ctx context.Context, req *pdpb.AllocIDRequest) (*pdpb.AllocIDResponse, error) {
	id := s.idAlloc.Alloc()
	return &pdpb.AllocIDResponse{
		Header: s.header(),
		Id:     id,
	}, nil
}

func (s *PDServer) GetStore(ctx context.Context, req *pdpb.GetStoreRequest) (*pdpb.GetStoreResponse, error) {
	store := s.meta.GetStore(req.GetStoreId())
	resp := &pdpb.GetStoreResponse{Header: s.header()}
	if store != nil {
		resp.Store = store
	}
	return resp, nil
}

func (s *PDServer) PutStore(ctx context.Context, req *pdpb.PutStoreRequest) (*pdpb.PutStoreResponse, error) {
	s.meta.PutStore(req.GetStore())
	return &pdpb.PutStoreResponse{Header: s.header()}, nil
}

func (s *PDServer) GetAllStores(ctx context.Context, req *pdpb.GetAllStoresRequest) (*pdpb.GetAllStoresResponse, error) {
	stores := s.meta.GetAllStores()
	return &pdpb.GetAllStoresResponse{
		Header: s.header(),
		Stores: stores,
	}, nil
}

func (s *PDServer) StoreHeartbeat(ctx context.Context, req *pdpb.StoreHeartbeatRequest) (*pdpb.StoreHeartbeatResponse, error) {
	if stats := req.GetStats(); stats != nil {
		s.meta.UpdateStoreStats(stats.GetStoreId(), stats)
	}
	return &pdpb.StoreHeartbeatResponse{Header: s.header()}, nil
}

func (s *PDServer) RegionHeartbeat(stream pdpb.PD_RegionHeartbeatServer) error {
	for {
		req, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}

		region := req.GetRegion()
		leader := req.GetLeader()
		if region != nil {
			s.meta.PutRegion(region, leader)
		}

		// Send empty response (no scheduling commands for now).
		resp := &pdpb.RegionHeartbeatResponse{Header: s.header()}
		if err := stream.Send(resp); err != nil {
			return err
		}
	}
}

func (s *PDServer) GetRegion(ctx context.Context, req *pdpb.GetRegionRequest) (*pdpb.GetRegionResponse, error) {
	region, leader := s.meta.GetRegionByKey(req.GetRegionKey())
	resp := &pdpb.GetRegionResponse{Header: s.header()}
	if region != nil {
		resp.Region = region
		resp.Leader = leader
	}
	return resp, nil
}

func (s *PDServer) GetRegionByID(ctx context.Context, req *pdpb.GetRegionByIDRequest) (*pdpb.GetRegionResponse, error) {
	region, leader := s.meta.GetRegionByID(req.GetRegionId())
	resp := &pdpb.GetRegionResponse{Header: s.header()}
	if region != nil {
		resp.Region = region
		resp.Leader = leader
	}
	return resp, nil
}

func (s *PDServer) AskBatchSplit(ctx context.Context, req *pdpb.AskBatchSplitRequest) (*pdpb.AskBatchSplitResponse, error) {
	resp := &pdpb.AskBatchSplitResponse{Header: s.header()}

	splitCount := int(req.GetSplitCount())
	if splitCount == 0 {
		splitCount = 1
	}
	for i := 0; i < splitCount; i++ {
		newRegionID := s.idAlloc.Alloc()
		var peerIDs []uint64
		for i := 0; i < s.cfg.MaxPeerCount; i++ {
			peerIDs = append(peerIDs, s.idAlloc.Alloc())
		}

		splitID := &pdpb.SplitID{
			NewRegionId: newRegionID,
			NewPeerIds:  peerIDs,
		}
		resp.Ids = append(resp.Ids, splitID)
	}

	return resp, nil
}

func (s *PDServer) ReportBatchSplit(ctx context.Context, req *pdpb.ReportBatchSplitRequest) (*pdpb.ReportBatchSplitResponse, error) {
	for _, region := range req.GetRegions() {
		s.meta.PutRegion(region, nil)
	}
	return &pdpb.ReportBatchSplitResponse{Header: s.header()}, nil
}

func (s *PDServer) GetGCSafePoint(ctx context.Context, req *pdpb.GetGCSafePointRequest) (*pdpb.GetGCSafePointResponse, error) {
	return &pdpb.GetGCSafePointResponse{
		Header:    s.header(),
		SafePoint: s.gcMgr.GetSafePoint(),
	}, nil
}

func (s *PDServer) UpdateGCSafePoint(ctx context.Context, req *pdpb.UpdateGCSafePointRequest) (*pdpb.UpdateGCSafePointResponse, error) {
	newSP := s.gcMgr.UpdateSafePoint(req.GetSafePoint())
	return &pdpb.UpdateGCSafePointResponse{
		Header:       s.header(),
		NewSafePoint: newSP,
	}, nil
}

func (s *PDServer) header() *pdpb.ResponseHeader {
	return &pdpb.ResponseHeader{
		ClusterId: s.clusterID,
	}
}

func (s *PDServer) errorHeader(msg string) *pdpb.ResponseHeader {
	return &pdpb.ResponseHeader{
		ClusterId: s.clusterID,
		Error: &pdpb.Error{
			Message: msg,
		},
	}
}

// --- Helper types ---

// MetadataStore manages cluster metadata in memory.
type MetadataStore struct {
	mu sync.RWMutex

	clusterID    uint64
	bootstrapped bool
	stores       map[uint64]*metapb.Store
	regions      map[uint64]*metapb.Region
	leaders      map[uint64]*metapb.Peer
	storeStats   map[uint64]*pdpb.StoreStats
}

func NewMetadataStore(clusterID uint64) *MetadataStore {
	return &MetadataStore{
		clusterID:  clusterID,
		stores:     make(map[uint64]*metapb.Store),
		regions:    make(map[uint64]*metapb.Region),
		leaders:    make(map[uint64]*metapb.Peer),
		storeStats: make(map[uint64]*pdpb.StoreStats),
	}
}

func (m *MetadataStore) IsBootstrapped() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.bootstrapped
}

func (m *MetadataStore) SetBootstrapped(v bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.bootstrapped = v
}

func (m *MetadataStore) PutStore(store *metapb.Store) {
	if store == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stores[store.GetId()] = store
}

func (m *MetadataStore) GetStore(id uint64) *metapb.Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.stores[id]
}

func (m *MetadataStore) GetAllStores() []*metapb.Store {
	m.mu.RLock()
	defer m.mu.RUnlock()
	stores := make([]*metapb.Store, 0, len(m.stores))
	for _, s := range m.stores {
		stores = append(stores, s)
	}
	return stores
}

func (m *MetadataStore) UpdateStoreStats(storeID uint64, stats *pdpb.StoreStats) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.storeStats[storeID] = stats
}

func (m *MetadataStore) PutRegion(region *metapb.Region, leader *metapb.Peer) {
	if region == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.regions[region.GetId()] = region
	if leader != nil {
		m.leaders[region.GetId()] = leader
	}
}

func (m *MetadataStore) GetRegionByID(id uint64) (*metapb.Region, *metapb.Peer) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.regions[id], m.leaders[id]
}

func (m *MetadataStore) GetRegionByKey(key []byte) (*metapb.Region, *metapb.Peer) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Linear scan for simplicity. In production, use a B-tree.
	for id, region := range m.regions {
		startKey := region.GetStartKey()
		endKey := region.GetEndKey()

		if len(startKey) > 0 && string(key) < string(startKey) {
			continue
		}
		if len(endKey) > 0 && string(key) >= string(endKey) {
			continue
		}
		return region, m.leaders[id]
	}
	return nil, nil
}

// --- TSO Allocator ---

// TSOAllocator generates monotonically increasing timestamps.
type TSOAllocator struct {
	mu           sync.Mutex
	physical     int64 // milliseconds since epoch
	logical      int64
	saveInterval time.Duration
}

func NewTSOAllocator(saveInterval time.Duration) *TSOAllocator {
	if saveInterval == 0 {
		saveInterval = 3 * time.Second
	}
	return &TSOAllocator{
		saveInterval: saveInterval,
	}
}

// Allocate allocates `count` timestamps and returns the last one.
func (t *TSOAllocator) Allocate(count int) (*pdpb.Timestamp, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := time.Now().UnixMilli()
	if now > t.physical {
		t.physical = now
		t.logical = 0
	}

	t.logical += int64(count)
	if t.logical >= (1 << 18) {
		// Overflow: advance physical.
		t.physical++
		t.logical = int64(count)
	}

	return &pdpb.Timestamp{
		Physical: t.physical,
		Logical:  int64(t.logical),
	}, nil
}

// --- ID Allocator ---

// IDAllocator provides monotonically increasing unique IDs.
type IDAllocator struct {
	mu     sync.Mutex
	nextID uint64
}

func NewIDAllocator() *IDAllocator {
	return &IDAllocator{nextID: 1000} // Start from 1000 to avoid collisions with bootstrap IDs.
}

// Alloc allocates the next ID.
func (a *IDAllocator) Alloc() uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := a.nextID
	a.nextID++
	return id
}

// --- GC Safe Point Manager ---

// GCSafePointManager manages the GC safe point.
type GCSafePointManager struct {
	mu        sync.Mutex
	safePoint uint64
}

func NewGCSafePointManager() *GCSafePointManager {
	return &GCSafePointManager{}
}

func (g *GCSafePointManager) GetSafePoint() uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.safePoint
}

func (g *GCSafePointManager) UpdateSafePoint(newSP uint64) uint64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	if newSP > g.safePoint {
		g.safePoint = newSP
	}
	return g.safePoint
}
