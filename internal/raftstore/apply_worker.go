package raftstore

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"

	"go.etcd.io/etcd/raft/v3/raftpb"
)

// ApplyTask carries committed entries for async application by the ApplyWorkerPool.
// It contains all committed data entries from a single handleReady cycle for one
// region, along with the information needed to invoke callbacks and report the
// result back to the peer.
type ApplyTask struct {
	// RegionID identifies the region these entries belong to.
	RegionID uint64

	// Entries are the committed data entries to apply (admin entries excluded).
	Entries []raftpb.Entry

	// ApplyFunc applies data entries to the KV engine.
	// Must not read or write any Peer fields.
	ApplyFunc func(regionID uint64, entries []raftpb.Entry)

	// Callbacks maps proposalID -> proposalEntry for entries in this batch.
	// The worker invokes these after successful engine write.
	Callbacks map[uint64]proposalEntry

	// CurrentTerm is the peer's Raft term when the entries were committed.
	CurrentTerm uint64

	// ResultCh is the peer's mailbox for sending back ApplyResult.
	ResultCh chan<- PeerMsg

	// LastCommittedIndex is the Index of the last entry in rd.CommittedEntries
	// (including admin entries). The apply worker uses this as the AppliedIndex
	// in the result, ensuring the applied index advances even when the batch
	// ends with admin entries.
	LastCommittedIndex uint64
}

// ApplyWorkerPool processes ApplyTasks in background goroutines.
// Modeled after flow.ReadPool: a fixed number of workers draining a shared
// task channel.
type ApplyWorkerPool struct {
	workers int
	taskCh  chan *ApplyTask
	stopCh  chan struct{}
	stopped atomic.Bool
	wg      sync.WaitGroup
}

// NewApplyWorkerPool creates a pool with the given number of workers.
func NewApplyWorkerPool(workers int) *ApplyWorkerPool {
	if workers <= 0 {
		workers = 4
	}
	pool := &ApplyWorkerPool{
		workers: workers,
		taskCh:  make(chan *ApplyTask, workers*16),
		stopCh:  make(chan struct{}),
	}
	pool.wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer pool.wg.Done()
			pool.worker()
		}()
	}
	return pool
}

// Submit sends an ApplyTask for background execution.
// Returns an error if the pool has been stopped.
func (p *ApplyWorkerPool) Submit(task *ApplyTask) error {
	if p.stopped.Load() {
		return fmt.Errorf("apply worker pool is stopped")
	}
	p.taskCh <- task
	return nil
}

// Stop signals all workers to drain remaining tasks and exit.
func (p *ApplyWorkerPool) Stop() {
	p.stopped.Store(true)
	close(p.stopCh)
	p.wg.Wait()
}

func (p *ApplyWorkerPool) worker() {
	for {
		select {
		case task := <-p.taskCh:
			p.processTask(task)
		case <-p.stopCh:
			// Drain remaining tasks before exiting.
			for {
				select {
				case task := <-p.taskCh:
					p.processTask(task)
				default:
					return
				}
			}
		}
	}
}

func (p *ApplyWorkerPool) processTask(task *ApplyTask) {
	// 1. Apply data entries to KV engine.
	if task.ApplyFunc != nil && len(task.Entries) > 0 {
		task.ApplyFunc(task.RegionID, task.Entries)
	}

	// 2. Invoke proposal callbacks.
	for _, e := range task.Entries {
		if e.Type != raftpb.EntryNormal || len(e.Data) < 8 {
			continue
		}
		proposalID := binary.BigEndian.Uint64(e.Data[:8])
		if proposalID == 0 {
			continue
		}
		if entry, ok := task.Callbacks[proposalID]; ok {
			if e.Term == entry.term {
				entry.callback(nil) // success
			} else {
				entry.callback(errorResponse(
					fmt.Errorf("term mismatch: proposed in %d, committed in %d",
						entry.term, e.Term)))
			}
		}
	}

	// 3. Compute applied index and send result back to peer.
	// Use LastCommittedIndex to ensure the applied index advances past
	// admin entries that were processed inline by the peer.
	appliedIndex := task.LastCommittedIndex

	if task.ResultCh != nil {
		result := &ApplyResult{
			RegionID:     task.RegionID,
			AppliedIndex: appliedIndex,
		}
		task.ResultCh <- PeerMsg{
			Type: PeerMsgTypeApplyResult,
			Data: result,
		}
	} else {
		slog.Warn("apply worker: no ResultCh for task",
			"region", task.RegionID)
	}
}
