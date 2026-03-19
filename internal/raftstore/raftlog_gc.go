package raftstore

import (
	"encoding/binary"
	"fmt"

	"github.com/ryogrid/gookv/internal/engine/traits"
	"github.com/ryogrid/gookv/pkg/cfnames"
	"github.com/ryogrid/gookv/pkg/keys"
)

// execCompactLog validates and applies a CompactLog admin command.
// It updates the ApplyState's TruncatedIndex and TruncatedTerm.
// Returns nil result if the compaction is a no-op (already compacted past this point).
func execCompactLog(applyState *ApplyState, req CompactLogRequest) (*CompactLogResult, error) {
	firstIndex := applyState.TruncatedIndex + 1
	if req.CompactIndex <= applyState.TruncatedIndex {
		// Already compacted past this point.
		return nil, nil
	}
	if req.CompactTerm == 0 {
		return nil, fmt.Errorf("raftstore: compact term is zero")
	}
	if req.CompactIndex > applyState.AppliedIndex {
		return nil, fmt.Errorf("raftstore: compact index %d > applied index %d",
			req.CompactIndex, applyState.AppliedIndex)
	}

	oldFirstIndex := firstIndex
	applyState.TruncatedIndex = req.CompactIndex
	applyState.TruncatedTerm = req.CompactTerm

	return &CompactLogResult{
		TruncatedIndex: req.CompactIndex,
		TruncatedTerm:  req.CompactTerm,
		FirstIndex:     oldFirstIndex,
	}, nil
}

// RaftLogGCWorker runs as a background goroutine deleting compacted Raft log entries.
type RaftLogGCWorker struct {
	engine traits.KvEngine
	taskCh <-chan RaftLogGCTask
	stopCh <-chan struct{}
}

// NewRaftLogGCWorker creates a new RaftLogGCWorker.
func NewRaftLogGCWorker(engine traits.KvEngine, taskCh <-chan RaftLogGCTask, stopCh <-chan struct{}) *RaftLogGCWorker {
	return &RaftLogGCWorker{
		engine: engine,
		taskCh: taskCh,
		stopCh: stopCh,
	}
}

// Run processes log deletion tasks until the stop channel is closed.
func (w *RaftLogGCWorker) Run() {
	for {
		select {
		case <-w.stopCh:
			return
		case task, ok := <-w.taskCh:
			if !ok {
				return
			}
			// Best-effort: errors are logged but do not stop the worker.
			_ = w.gcRaftLog(task)
		}
	}
}

// gcRaftLog deletes entries in [startIdx, endIdx) for a region.
func (w *RaftLogGCWorker) gcRaftLog(task RaftLogGCTask) error {
	if task.StartIdx >= task.EndIdx {
		return nil
	}

	startKey := keys.RaftLogKey(task.RegionID, task.StartIdx)
	endKey := keys.RaftLogKey(task.RegionID, task.EndIdx)

	if err := w.engine.DeleteRange(cfnames.CFRaft, startKey, endKey); err != nil {
		return fmt.Errorf("raftstore: gc raft log region %d [%d, %d): %w",
			task.RegionID, task.StartIdx, task.EndIdx, err)
	}
	return nil
}

// marshalCompactLogRequest serializes a CompactLogRequest for Raft proposal.
func marshalCompactLogRequest(req CompactLogRequest) []byte {
	data := make([]byte, 17)
	data[0] = 0x01 // tag: CompactLog admin command
	binary.BigEndian.PutUint64(data[1:9], req.CompactIndex)
	binary.BigEndian.PutUint64(data[9:17], req.CompactTerm)
	return data
}

// unmarshalCompactLogRequest deserializes a CompactLogRequest.
func unmarshalCompactLogRequest(data []byte) (CompactLogRequest, bool) {
	if len(data) < 17 || data[0] != 0x01 {
		return CompactLogRequest{}, false
	}
	return CompactLogRequest{
		CompactIndex: binary.BigEndian.Uint64(data[1:9]),
		CompactTerm:  binary.BigEndian.Uint64(data[9:17]),
	}, true
}
