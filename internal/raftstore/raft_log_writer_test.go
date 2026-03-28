package raftstore

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/ryogrid/gookv/internal/engine/rocks"
	"github.com/ryogrid/gookv/pkg/cfnames"
)

func newTestWriter(t *testing.T) (*RaftLogWriter, *rocks.Engine) {
	t.Helper()
	dir := t.TempDir()
	engine, err := rocks.Open(dir)
	require.NoError(t, err)
	t.Cleanup(func() { engine.Close() })

	writer := NewRaftLogWriter(engine, 64)
	t.Cleanup(func() { writer.Stop() })

	return writer, engine
}

func TestRaftLogWriter_SingleTask(t *testing.T) {
	writer, engine := newTestWriter(t)

	task := &WriteTask{
		RegionID: 1,
		Ops: []WriteOp{
			{CF: cfnames.CFRaft, Key: []byte("key1"), Value: []byte("val1")},
			{CF: cfnames.CFRaft, Key: []byte("key2"), Value: []byte("val2")},
		},
		Done: make(chan error, 1),
	}

	writer.Submit(task)
	err := <-task.Done
	require.NoError(t, err)

	// Verify data persisted.
	val, err := engine.Get(cfnames.CFRaft, []byte("key1"))
	require.NoError(t, err)
	assert.Equal(t, []byte("val1"), val)

	val, err = engine.Get(cfnames.CFRaft, []byte("key2"))
	require.NoError(t, err)
	assert.Equal(t, []byte("val2"), val)
}

func TestRaftLogWriter_BatchedTasks(t *testing.T) {
	writer, engine := newTestWriter(t)

	const numTasks = 10
	var wg sync.WaitGroup
	tasks := make([]*WriteTask, numTasks)

	for i := 0; i < numTasks; i++ {
		tasks[i] = &WriteTask{
			RegionID: uint64(i + 1),
			Ops: []WriteOp{
				{CF: cfnames.CFRaft, Key: []byte{byte(i)}, Value: []byte{byte(i + 100)}},
			},
			Done: make(chan error, 1),
		}
	}

	// Submit all tasks concurrently to encourage batching.
	for i := 0; i < numTasks; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			writer.Submit(tasks[idx])
		}(i)
	}

	// Wait for all submissions.
	wg.Wait()

	// Wait for all Done channels.
	for i := 0; i < numTasks; i++ {
		err := <-tasks[i].Done
		require.NoError(t, err, "task %d", i)
	}

	// Verify all data persisted.
	for i := 0; i < numTasks; i++ {
		val, err := engine.Get(cfnames.CFRaft, []byte{byte(i)})
		require.NoError(t, err, "task %d", i)
		assert.Equal(t, []byte{byte(i + 100)}, val)
	}
}

func TestRaftLogWriter_StopDrainsRemaining(t *testing.T) {
	dir := t.TempDir()
	engine, err := rocks.Open(dir)
	require.NoError(t, err)
	defer engine.Close()

	writer := NewRaftLogWriter(engine, 64)

	// Submit a task and immediately stop.
	task := &WriteTask{
		RegionID: 1,
		Ops: []WriteOp{
			{CF: cfnames.CFRaft, Key: []byte("drain-key"), Value: []byte("drain-val")},
		},
		Done: make(chan error, 1),
	}
	writer.Submit(task)

	// Stop should drain the task.
	writer.Stop()

	select {
	case err := <-task.Done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("task.Done not signaled after Stop")
	}

	val, err := engine.Get(cfnames.CFRaft, []byte("drain-key"))
	require.NoError(t, err)
	assert.Equal(t, []byte("drain-val"), val)
}
