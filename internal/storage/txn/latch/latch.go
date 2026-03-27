// Package latch provides deadlock-free key serialization for the transaction scheduler.
// Commands that touch overlapping keys are serialized through latches to prevent
// concurrent modification of the same keys.
package latch

import (
	"context"
	"hash/fnv"
	"sort"
	"sync"
)

// Latches provides deadlock-free key serialization using hash-based slots.
type Latches struct {
	slots   []latchSlot
	size    int
	waiters sync.Map // commandID -> chan struct{}
}

// latchSlot is a single latch slot that can be held by one command at a time.
type latchSlot struct {
	mu        sync.Mutex
	owner     uint64   // Command ID of the current owner (0 = free).
	waitQueue []uint64 // Command IDs waiting for this slot.
}

// Lock tracks latch acquisition progress for a command.
type Lock struct {
	RequiredHashes []uint64 // Sorted, deduplicated key hashes.
	OwnedCount     int      // How many latches have been acquired so far.
}

// New creates a Latches instance with the given number of slots.
// The slot count is rounded up to the nearest power of 2.
func New(slotCount int) *Latches {
	// Round up to power of 2.
	size := 1
	for size < slotCount {
		size <<= 1
	}

	slots := make([]latchSlot, size)

	return &Latches{
		slots: slots,
		size:  size,
	}
}

// GenLock creates a Lock from a set of keys. The key hashes are sorted
// and deduplicated to ensure deadlock-free acquisition.
func (l *Latches) GenLock(keys [][]byte) *Lock {
	hashes := make([]uint64, 0, len(keys))
	seen := make(map[uint64]bool)

	for _, key := range keys {
		h := hashKey(key)
		slotIdx := h & uint64(l.size-1)
		if !seen[slotIdx] {
			seen[slotIdx] = true
			hashes = append(hashes, slotIdx)
		}
	}

	sort.Slice(hashes, func(i, j int) bool {
		return hashes[i] < hashes[j]
	})

	return &Lock{
		RequiredHashes: hashes,
		OwnedCount:     0,
	}
}

// Acquire attempts to acquire all latches for a command.
// Returns true if all latches were acquired. If false, the command must wait
// and retry when notified.
func (l *Latches) Acquire(lock *Lock, commandID uint64) bool {
	for i := lock.OwnedCount; i < len(lock.RequiredHashes); i++ {
		slotIdx := lock.RequiredHashes[i]
		slot := &l.slots[slotIdx]

		slot.mu.Lock()
		if slot.owner == 0 || slot.owner == commandID {
			// Slot is free or already owned by this command.
			slot.owner = commandID
			lock.OwnedCount = i + 1
			slot.mu.Unlock()
		} else {
			// Slot is owned by another command. Add to wait queue.
			slot.waitQueue = append(slot.waitQueue, commandID)
			slot.mu.Unlock()
			return false
		}
	}
	return true
}

// AcquireBlocking acquires all latches, blocking if necessary.
// Returns when all latches are held. The context can be used for timeout/cancellation.
func (l *Latches) AcquireBlocking(ctx context.Context, lock *Lock, commandID uint64) error {
	// Create and store the channel BEFORE calling Acquire, so Release()
	// can always find it if the command ends up in a waitQueue.
	ch := make(chan struct{}, 1)
	l.waiters.Store(commandID, ch)

	for {
		if l.Acquire(lock, commandID) {
			l.waiters.Delete(commandID)
			return nil
		}
		select {
		case <-ch:
			// Woken up -- retry acquisition.
			continue
		case <-ctx.Done():
			// Timeout or cancellation -- release any partially acquired latches.
			l.Release(lock, commandID)
			l.waiters.Delete(commandID)
			return ctx.Err()
		}
	}
}

// Release releases all latches held by a command, returning the IDs of
// commands that should be woken up (they can now retry acquisition).
func (l *Latches) Release(lock *Lock, commandID uint64) []uint64 {
	var wakeUp []uint64

	for i := 0; i < lock.OwnedCount; i++ {
		slotIdx := lock.RequiredHashes[i]
		slot := &l.slots[slotIdx]

		slot.mu.Lock()
		if slot.owner == commandID {
			slot.owner = 0
			// Wake the first waiter.
			if len(slot.waitQueue) > 0 {
				waiter := slot.waitQueue[0]
				slot.waitQueue = slot.waitQueue[1:]
				wakeUp = append(wakeUp, waiter)
			}
		}
		slot.mu.Unlock()
	}

	lock.OwnedCount = 0

	// Signal woken commands via their per-command channels.
	for _, id := range wakeUp {
		if v, ok := l.waiters.Load(id); ok {
			ch := v.(chan struct{})
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}

	return wakeUp
}

// hashKey computes an FNV-1a hash of a key.
func hashKey(key []byte) uint64 {
	h := fnv.New64a()
	h.Write(key)
	return h.Sum64()
}
