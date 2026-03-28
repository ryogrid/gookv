package raftstore

import (
	"fmt"
	"log/slog"
	"sync"

	"github.com/ryogrid/gookv/internal/engine/traits"
	"go.etcd.io/etcd/raft/v3/raftpb"
)

// WriteOp represents a single key-value write operation for Raft log persistence.
type WriteOp struct {
	CF    string
	Key   []byte
	Value []byte
}

// WriteTask carries one region's Raft log changes for batch persistence.
// The peer builds this from a raft.Ready and submits it to the RaftLogWriter.
// After persistence completes, Done is signaled and the peer updates in-memory state.
type WriteTask struct {
	RegionID  uint64
	Ops       []WriteOp        // serialized entries + hard state
	Done      chan error        // signaled after persistence completes
	Entries   []raftpb.Entry   // for in-memory cache update after persist
	HardState *raftpb.HardState // for in-memory hard state update after persist
}

// RaftLogWriter batches WriteTask submissions from multiple peer goroutines
// into a single WriteBatch + fsync per cycle. This reduces the number of
// fsyncs from N (one per region) to 1 per batch cycle.
type RaftLogWriter struct {
	engine traits.KvEngine
	taskCh chan *WriteTask
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewRaftLogWriter creates and starts the writer goroutine.
func NewRaftLogWriter(engine traits.KvEngine, chanSize int) *RaftLogWriter {
	if chanSize <= 0 {
		chanSize = 256
	}
	w := &RaftLogWriter{
		engine: engine,
		taskCh: make(chan *WriteTask, chanSize),
		stopCh: make(chan struct{}),
	}
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()
		w.run()
	}()
	return w
}

// Submit sends a WriteTask to the writer. The caller must wait on task.Done.
func (w *RaftLogWriter) Submit(task *WriteTask) {
	w.taskCh <- task
}

// Stop signals the writer to stop and waits for it to finish.
// All pending tasks are drained before returning.
func (w *RaftLogWriter) Stop() {
	close(w.stopCh)
	w.wg.Wait()
}

func (w *RaftLogWriter) run() {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("RaftLogWriter: panic recovered", "panic", fmt.Sprintf("%v", r))
			// Drain remaining tasks with error.
			w.failRemaining(fmt.Errorf("RaftLogWriter panicked: %v", r))
		}
	}()

	for {
		select {
		case task := <-w.taskCh:
			w.processBatch(task)
		case <-w.stopCh:
			w.drainRemaining()
			return
		}
	}
}

func (w *RaftLogWriter) processBatch(first *WriteTask) {
	// Collect the first task plus any others already queued.
	tasks := []*WriteTask{first}
	for {
		select {
		case task := <-w.taskCh:
			tasks = append(tasks, task)
		default:
			goto commit
		}
	}

commit:
	// Build a single WriteBatch from all tasks.
	wb := w.engine.NewWriteBatch()
	for _, task := range tasks {
		for _, op := range task.Ops {
			if err := wb.Put(op.CF, op.Key, op.Value); err != nil {
				// Fail ALL tasks in the batch (not partial commit).
				for _, t := range tasks {
					t.Done <- err
				}
				return
			}
		}
	}

	// Single fsync for the entire batch.
	err := wb.Commit()
	if err != nil {
		slog.Error("RaftLogWriter: batch commit failed", "err", err,
			"regions", len(tasks))
	}

	// Notify all waiting peers.
	for _, task := range tasks {
		task.Done <- err
	}
}

func (w *RaftLogWriter) drainRemaining() {
	for {
		select {
		case task := <-w.taskCh:
			w.processBatch(task)
		default:
			return
		}
	}
}

func (w *RaftLogWriter) failRemaining(err error) {
	for {
		select {
		case task := <-w.taskCh:
			task.Done <- err
		default:
			return
		}
	}
}
