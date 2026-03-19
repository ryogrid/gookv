package server

import (
	"github.com/pingcap/kvproto/pkg/raft_cmdpb"
	"github.com/ryogrid/gookv/internal/storage/mvcc"
)

// ModifiesToRequests converts MVCC Modify operations to raft_cmdpb.Request entries.
// This is the serialization path: leader computes modifications, then proposes them
// as protobuf Request entries via Raft.
func ModifiesToRequests(modifies []mvcc.Modify) []*raft_cmdpb.Request {
	reqs := make([]*raft_cmdpb.Request, 0, len(modifies))
	for _, m := range modifies {
		switch m.Type {
		case mvcc.ModifyTypePut:
			reqs = append(reqs, &raft_cmdpb.Request{
				CmdType: raft_cmdpb.CmdType_Put,
				Put: &raft_cmdpb.PutRequest{
					Cf:    m.CF,
					Key:   m.Key,
					Value: m.Value,
				},
			})
		case mvcc.ModifyTypeDelete:
			reqs = append(reqs, &raft_cmdpb.Request{
				CmdType: raft_cmdpb.CmdType_Delete,
				Delete: &raft_cmdpb.DeleteRequest{
					Cf:  m.CF,
					Key: m.Key,
				},
			})
		}
	}
	return reqs
}

// RequestsToModifies converts raft_cmdpb.Request entries back to MVCC Modify operations.
// This is the deserialization path: all nodes apply committed entries to their engines.
func RequestsToModifies(reqs []*raft_cmdpb.Request) []mvcc.Modify {
	modifies := make([]mvcc.Modify, 0, len(reqs))
	for _, r := range reqs {
		switch r.CmdType {
		case raft_cmdpb.CmdType_Put:
			modifies = append(modifies, mvcc.Modify{
				Type:  mvcc.ModifyTypePut,
				CF:    r.Put.Cf,
				Key:   r.Put.Key,
				Value: r.Put.Value,
			})
		case raft_cmdpb.CmdType_Delete:
			modifies = append(modifies, mvcc.Modify{
				Type: mvcc.ModifyTypeDelete,
				CF:   r.Delete.Cf,
				Key:  r.Delete.Key,
			})
		}
	}
	return modifies
}
