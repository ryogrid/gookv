package raftstore

import (
	"github.com/pingcap/kvproto/pkg/eraftpb"
	"go.etcd.io/etcd/raft/v3/raftpb"
)

// EraftpbToRaftpb converts a kvproto eraftpb.Message to etcd raftpb.Message
// using protobuf marshal/unmarshal since the wire format is compatible.
func EraftpbToRaftpb(src *eraftpb.Message) (raftpb.Message, error) {
	data, err := src.Marshal()
	if err != nil {
		return raftpb.Message{}, err
	}
	var dst raftpb.Message
	if err := dst.Unmarshal(data); err != nil {
		return raftpb.Message{}, err
	}
	return dst, nil
}

// RaftpbToEraftpb converts an etcd raftpb.Message to kvproto eraftpb.Message
// using protobuf marshal/unmarshal since the wire format is compatible.
func RaftpbToEraftpb(src *raftpb.Message) (*eraftpb.Message, error) {
	data, err := src.Marshal()
	if err != nil {
		return nil, err
	}
	dst := &eraftpb.Message{}
	if err := dst.Unmarshal(data); err != nil {
		return nil, err
	}
	return dst, nil
}
