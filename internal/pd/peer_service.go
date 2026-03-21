package pd

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/pingcap/kvproto/pkg/raft_serverpb"
	"google.golang.org/grpc"

	"github.com/ryogrid/gookv/internal/raftstore"
)

// PDPeerServiceServer is the interface that the PD peer gRPC service must implement.
type PDPeerServiceServer interface {
	SendPDRaftMessage(ctx context.Context, req *raft_serverpb.RaftMessage) (*raft_serverpb.RaftMessage, error)
}

// PDPeerService handles incoming PD Raft messages from other PD peers.
type PDPeerService struct {
	peer *PDRaftPeer
}

// SendPDRaftMessage receives a RaftMessage from a remote PD peer, converts it
// from eraftpb to raftpb format, and delivers it to the local peer's mailbox.
func (s *PDPeerService) SendPDRaftMessage(ctx context.Context, req *raft_serverpb.RaftMessage) (*raft_serverpb.RaftMessage, error) {
	if req.GetMessage() == nil {
		return &raft_serverpb.RaftMessage{}, nil
	}

	raftMsg, err := raftstore.EraftpbToRaftpb(req.GetMessage())
	if err != nil {
		return nil, fmt.Errorf("pd: convert eraftpb to raftpb: %w", err)
	}

	select {
	case s.peer.Mailbox <- PDRaftMsg{
		Type: PDRaftMsgTypeRaftMessage,
		Data: &raftMsg,
	}:
	default:
		slog.Warn("pd: peer mailbox full, dropping message",
			"node", s.peer.nodeID,
			"from", raftMsg.From,
			"type", raftMsg.Type,
		)
	}

	return &raft_serverpb.RaftMessage{}, nil
}

// RegisterPDPeerService registers the PD peer-to-peer Raft message service
// on the given gRPC server. Uses a hand-coded service descriptor (no proto
// code generation needed).
func RegisterPDPeerService(srv *grpc.Server, peer *PDRaftPeer) {
	svc := &PDPeerService{peer: peer}
	srv.RegisterService(&_PDPeer_serviceDesc, svc)
}

// _PDPeer_serviceDesc is the hand-coded gRPC service descriptor for the
// PD peer-to-peer Raft message service.
var _PDPeer_serviceDesc = grpc.ServiceDesc{
	ServiceName: "pd.PDPeer",
	HandlerType: (*PDPeerServiceServer)(nil),
	Methods: []grpc.MethodDesc{
		{
			MethodName: "SendPDRaftMessage",
			Handler:    _PDPeerService_SendPDRaftMessage_Handler,
		},
	},
	Streams:  []grpc.StreamDesc{},
	Metadata: "pd_peer.proto",
}

// _PDPeerService_SendPDRaftMessage_Handler is the unary handler for
// the SendPDRaftMessage RPC method.
func _PDPeerService_SendPDRaftMessage_Handler(
	srv interface{},
	ctx context.Context,
	dec func(interface{}) error,
	interceptor grpc.UnaryServerInterceptor,
) (interface{}, error) {
	req := &raft_serverpb.RaftMessage{}
	if err := dec(req); err != nil {
		return nil, err
	}

	if interceptor == nil {
		return srv.(*PDPeerService).SendPDRaftMessage(ctx, req)
	}

	info := &grpc.UnaryServerInfo{
		Server:     srv,
		FullMethod: "/pd.PDPeer/SendPDRaftMessage",
	}
	handler := func(ctx context.Context, req interface{}) (interface{}, error) {
		return srv.(*PDPeerService).SendPDRaftMessage(ctx, req.(*raft_serverpb.RaftMessage))
	}
	return interceptor(ctx, req, info, handler)
}

// Ensure PDPeerService satisfies the PDPeerServiceServer interface at compile time.
var _ PDPeerServiceServer = (*PDPeerService)(nil)
