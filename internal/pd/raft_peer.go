package pd

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	"go.etcd.io/etcd/raft/v3"
	"go.etcd.io/etcd/raft/v3/raftpb"

	"github.com/ryogrid/gookv/internal/raftstore"
)

// ErrNotLeader is returned when a proposal is attempted on a non-leader peer.
var ErrNotLeader = errors.New("pd: not leader")

// PDRaftMsgType identifies the type of message sent to a PDRaftPeer's mailbox.
type PDRaftMsgType int

const (
	// PDRaftMsgTypeRaftMessage carries a raftpb.Message from another peer.
	PDRaftMsgTypeRaftMessage PDRaftMsgType = iota
	// PDRaftMsgTypeProposal carries a PDProposal to be proposed to the Raft group.
	PDRaftMsgTypeProposal
)

// PDRaftMsg is a message delivered to a PDRaftPeer's Mailbox channel.
type PDRaftMsg struct {
	Type PDRaftMsgType
	Data interface{}
}

// PDProposal wraps a PDCommand with a callback for notification when the
// proposal is committed and applied.
type PDProposal struct {
	Command  PDCommand
	Callback func([]byte, error)
}

// PDRaftConfig holds configuration for a PDRaftPeer.
type PDRaftConfig struct {
	// RaftTickInterval is the base tick interval for Raft (default 100ms).
	RaftTickInterval time.Duration
	// ElectionTimeoutTicks is the election timeout in ticks (default 10).
	ElectionTimeoutTicks int
	// HeartbeatTicks is the heartbeat interval in ticks (default 2).
	HeartbeatTicks int
	// MaxInflightMsgs is the maximum number of in-flight messages (default 256).
	MaxInflightMsgs int
	// MaxSizePerMsg is the maximum size of a single Raft message (default 1 MiB).
	MaxSizePerMsg uint64
	// MailboxCapacity is the size of the peer's mailbox channel (default 256).
	MailboxCapacity int
	// RaftLogGCTickInterval is how often the log GC tick fires (default 60s).
	// Set to 0 to disable Raft log GC.
	RaftLogGCTickInterval time.Duration
	// RaftLogGCCountLimit triggers compaction when excess entry count exceeds this (default 10000).
	RaftLogGCCountLimit uint64
	// RaftLogGCThreshold is the minimum number of entries to keep after compaction (default 50).
	RaftLogGCThreshold uint64
}

// DefaultPDRaftConfig returns a PDRaftConfig with sensible defaults.
func DefaultPDRaftConfig() PDRaftConfig {
	return PDRaftConfig{
		RaftTickInterval:      100 * time.Millisecond,
		ElectionTimeoutTicks:  10,
		HeartbeatTicks:        2,
		MaxInflightMsgs:       256,
		MaxSizePerMsg:         1 << 20, // 1 MiB
		MailboxCapacity:       256,
		RaftLogGCTickInterval: 60 * time.Second,
		RaftLogGCCountLimit:   10000,
		RaftLogGCThreshold:    50,
	}
}

// PDRaftPeer manages a single Raft node in the PD cluster.
// It is modeled on Peer in internal/raftstore/peer.go but simplified:
// no region management, no conf change handling, no split checks.
type PDRaftPeer struct {
	nodeID    uint64
	rawNode   *raft.RawNode
	storage   *PDRaftStorage
	peerAddrs map[uint64]string

	// Mailbox receives PDRaftMsg from other peers and from ProposeAndWait.
	Mailbox chan PDRaftMsg

	// sendFunc dispatches outgoing Raft messages to other peers.
	sendFunc func([]raftpb.Message)

	// pendingProposals tracks in-flight proposals in FIFO order.
	// Every call to propose() appends one entry (nil or non-nil callback).
	// Only accessed from the Run goroutine.
	pendingProposals []func([]byte, error)

	// applyFunc is called for each committed entry to apply the PDCommand
	// to the PD state machine. It returns a result and optional error.
	applyFunc func(PDCommand) ([]byte, error)

	// applySnapshotFunc is called when a snapshot is received from the leader.
	// It replaces the PD server's in-memory state with the snapshot data.
	applySnapshotFunc func([]byte) error

	// leaderChangeFunc is called when the local leader status changes.
	// The argument is true if this node became the leader, false otherwise.
	leaderChangeFunc func(isLeader bool)

	isLeader atomic.Bool
	leaderID atomic.Uint64
	stopped  atomic.Bool

	// lastCompactedIdx tracks the last index scheduled for log GC.
	lastCompactedIdx uint64

	cfg PDRaftConfig
}

// NewPDRaftPeer creates a new PDRaftPeer for the given node.
// If peers is non-nil, the Raft group is bootstrapped with the given peer list.
// If peers is nil, the storage must have been recovered via RecoverFromEngine().
func NewPDRaftPeer(
	nodeID uint64,
	storage *PDRaftStorage,
	peers []raft.Peer,
	peerAddrs map[uint64]string,
	cfg PDRaftConfig,
) (*PDRaftPeer, error) {
	if len(peers) > 0 {
		// Bootstrap: set up empty storage matching MemoryStorage convention.
		storage.SetApplyState(raftstore.ApplyState{
			AppliedIndex:   0,
			TruncatedIndex: 0,
			TruncatedTerm:  0,
		})
		storage.SetPersistedLastIndex(0)
		storage.SetDummyEntry()
	}

	raftCfg := &raft.Config{
		ID:              nodeID,
		ElectionTick:    cfg.ElectionTimeoutTicks,
		HeartbeatTick:   cfg.HeartbeatTicks,
		Storage:         storage,
		MaxInflightMsgs: cfg.MaxInflightMsgs,
		MaxSizePerMsg:   cfg.MaxSizePerMsg,
		CheckQuorum:     true,
		PreVote:         true,
	}

	rawNode, err := raft.NewRawNode(raftCfg)
	if err != nil {
		return nil, fmt.Errorf("pd: new raw node: %w", err)
	}

	if len(peers) > 0 {
		if err := rawNode.Bootstrap(peers); err != nil {
			return nil, fmt.Errorf("pd: bootstrap: %w", err)
		}
	}

	p := &PDRaftPeer{
		nodeID:           nodeID,
		rawNode:          rawNode,
		storage:          storage,
		peerAddrs:        peerAddrs,
		Mailbox:          make(chan PDRaftMsg, cfg.MailboxCapacity),
		pendingProposals: nil, // zero-value slice; appended in propose()
		cfg:              cfg,
	}

	return p, nil
}

// Run starts the peer's main event loop. Blocks until ctx is cancelled
// or the peer is stopped.
func (p *PDRaftPeer) Run(ctx context.Context) {
	ticker := time.NewTicker(p.cfg.RaftTickInterval)
	defer ticker.Stop()

	// Optional GC ticker for Raft log compaction.
	var gcTickerCh <-chan time.Time
	if p.cfg.RaftLogGCTickInterval > 0 {
		gcTicker := time.NewTicker(p.cfg.RaftLogGCTickInterval)
		defer gcTicker.Stop()
		gcTickerCh = gcTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			p.stopped.Store(true)
			return

		case <-ticker.C:
			p.rawNode.Tick()

		case <-gcTickerCh:
			p.onRaftLogGCTick()

		case msg, ok := <-p.Mailbox:
			if !ok {
				p.stopped.Store(true)
				return
			}
			p.handleMessage(msg)
		}

		p.handleReady()
	}
}

// handleMessage processes a single PDRaftMsg.
func (p *PDRaftPeer) handleMessage(msg PDRaftMsg) {
	switch msg.Type {
	case PDRaftMsgTypeRaftMessage:
		raftMsg := msg.Data.(*raftpb.Message)
		if err := p.rawNode.Step(*raftMsg); err != nil {
			// Log but continue; stale/invalid messages are normal.
			_ = err
		}

	case PDRaftMsgTypeProposal:
		proposal := msg.Data.(*PDProposal)
		p.propose(proposal)
	}
}

// propose marshals and proposes a PDCommand to the Raft group,
// tracking the callback for when the entry is committed.
func (p *PDRaftPeer) propose(proposal *PDProposal) {
	data, err := proposal.Command.Marshal()
	if err != nil {
		if proposal.Callback != nil {
			proposal.Callback(nil, fmt.Errorf("pd: marshal command: %w", err))
		}
		return
	}

	if err := p.rawNode.Propose(data); err != nil {
		if proposal.Callback != nil {
			proposal.Callback(nil, fmt.Errorf("pd: propose: %w", err))
		}
		return
	}

	// Always append to maintain FIFO alignment, even if callback is nil.
	// Nil callbacks (e.g. CmdCompactLog) are discarded during dequeue.
	p.pendingProposals = append(p.pendingProposals, proposal.Callback)
}

// handleReady processes a Raft Ready batch, following the same 7-step pattern
// as Peer.handleReady() in internal/raftstore/peer.go.
func (p *PDRaftPeer) handleReady() {
	if !p.rawNode.HasReady() {
		return
	}

	rd := p.rawNode.Ready()

	// Step 1: Update leader status from SoftState.
	if rd.SoftState != nil {
		wasLeader := p.isLeader.Load()
		nowLeader := rd.SoftState.Lead == p.nodeID
		p.isLeader.Store(nowLeader)
		p.leaderID.Store(rd.SoftState.Lead)

		// Fire leader change callback when status transitions.
		if wasLeader != nowLeader && p.leaderChangeFunc != nil {
			p.leaderChangeFunc(nowLeader)
		}

		// If we lost leadership, drain all pending proposals with ErrNotLeader.
		if wasLeader && !nowLeader {
			for _, cb := range p.pendingProposals {
				if cb != nil {
					cb(nil, ErrNotLeader)
				}
			}
			p.pendingProposals = nil
		}
	}

	// Step 1.5: Apply incoming snapshot if present.
	if !raft.IsEmptySnap(rd.Snapshot) && len(rd.Snapshot.Data) > 0 {
		if p.applySnapshotFunc != nil {
			if err := p.applySnapshotFunc(rd.Snapshot.Data); err != nil {
				slog.Error("pd: failed to apply snapshot",
					"node", p.nodeID, "err", err)
			} else {
				// Update apply state to match snapshot metadata.
				as := p.storage.GetApplyState()
				as.AppliedIndex = rd.Snapshot.Metadata.Index
				as.TruncatedIndex = rd.Snapshot.Metadata.Index
				as.TruncatedTerm = rd.Snapshot.Metadata.Term
				p.storage.SetApplyState(as)
				slog.Info("pd: applied snapshot",
					"node", p.nodeID,
					"snapIndex", rd.Snapshot.Metadata.Index,
					"snapTerm", rd.Snapshot.Metadata.Term)
			}
		}
	}

	// Step 2: Persist entries and hard state.
	if err := p.storage.SaveReady(rd); err != nil {
		slog.Error("pd: failed to save ready", "node", p.nodeID, "err", err)
		return
	}

	// Step 3: Send Raft messages to other peers.
	if p.sendFunc != nil && len(rd.Messages) > 0 {
		p.sendFunc(rd.Messages)
	}

	// Step 4: Apply committed entries.
	if len(rd.CommittedEntries) > 0 {
		for _, e := range rd.CommittedEntries {
			// No-op entries (leader election) are not proposed via propose(),
			// so do NOT dequeue from pendingProposals.
			if len(e.Data) == 0 {
				continue
			}

			// ConfChange entries are not proposed via propose(),
			// so do NOT dequeue from pendingProposals.
			if e.Type == raftpb.EntryConfChange || e.Type == raftpb.EntryConfChangeV2 {
				continue
			}

			cmd, err := UnmarshalPDCommand(e.Data)
			if err != nil {
				slog.Error("pd: failed to unmarshal committed entry",
					"node", p.nodeID, "index", e.Index, "err", err)
				// Dequeue and notify error if this is our proposal.
				if len(p.pendingProposals) > 0 {
					cb := p.pendingProposals[0]
					p.pendingProposals = p.pendingProposals[1:]
					if cb != nil {
						cb(nil, err)
					}
				}
				continue
			}

			// Apply to state machine.
			var result []byte
			var applyErr error
			if p.applyFunc != nil {
				result, applyErr = p.applyFunc(cmd)
			}

			// Dequeue the next pending callback (FIFO).
			if len(p.pendingProposals) > 0 {
				cb := p.pendingProposals[0]
				p.pendingProposals = p.pendingProposals[1:]
				if cb != nil {
					cb(result, applyErr)
				}
			}
		}

		// Update applied index to the last committed entry.
		lastCommitted := rd.CommittedEntries[len(rd.CommittedEntries)-1]
		as := p.storage.GetApplyState()
		as.AppliedIndex = lastCommitted.Index
		p.storage.SetApplyState(as)
	}

	// Step 5: Advance the Raft state machine.
	p.rawNode.Advance(rd)
}

// onRaftLogGCTick evaluates whether the Raft log should be compacted.
// Only the leader proposes CmdCompactLog commands.
// Follows the same pattern as Peer.onRaftLogGCTick in internal/raftstore/peer.go,
// simplified for single-node PD (no follower match tracking).
func (p *PDRaftPeer) onRaftLogGCTick() {
	if !p.isLeader.Load() {
		return
	}

	as := p.storage.GetApplyState()
	appliedIdx := as.AppliedIndex
	firstIdx := as.TruncatedIndex + 1

	if appliedIdx <= firstIdx {
		return
	}

	excessCount := appliedIdx - firstIdx
	if excessCount < p.cfg.RaftLogGCCountLimit {
		return
	}

	compactIdx := appliedIdx - p.cfg.RaftLogGCThreshold
	if compactIdx <= p.lastCompactedIdx {
		return
	}

	// Get the term at compactIdx.
	term, err := p.storage.Term(compactIdx)
	if err != nil {
		return
	}

	cmd := PDCommand{
		Type:         CmdCompactLog,
		CompactIndex: compactIdx,
		CompactTerm:  term,
	}
	// Route through propose() with nil callback to keep the FIFO queue
	// aligned. Direct rawNode.Propose() would cause callback mismatches.
	p.handleMessage(PDRaftMsg{
		Type: PDRaftMsgTypeProposal,
		Data: &PDProposal{
			Command:  cmd,
			Callback: nil,
		},
	})
	p.lastCompactedIdx = compactIdx
}

// ProposeAndWait proposes a PDCommand and blocks until it is committed and applied,
// or the context is cancelled. Returns ErrNotLeader if this peer is not the leader.
func (p *PDRaftPeer) ProposeAndWait(ctx context.Context, cmd PDCommand) ([]byte, error) {
	if !p.isLeader.Load() {
		return nil, ErrNotLeader
	}

	type callbackResult struct {
		data []byte
		err  error
	}
	ch := make(chan callbackResult, 1)

	proposal := &PDProposal{
		Command: cmd,
		Callback: func(data []byte, err error) {
			ch <- callbackResult{data: data, err: err}
		},
	}

	// Send proposal to the event loop via mailbox.
	select {
	case p.Mailbox <- PDRaftMsg{Type: PDRaftMsgTypeProposal, Data: proposal}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	// Wait for the callback or context cancellation.
	select {
	case result := <-ch:
		return result.data, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// IsLeader returns whether this peer believes it is the Raft leader.
func (p *PDRaftPeer) IsLeader() bool { return p.isLeader.Load() }

// LeaderID returns the current leader's node ID. Returns 0 if unknown.
func (p *PDRaftPeer) LeaderID() uint64 { return p.leaderID.Load() }

// SetSendFunc sets the function used to send Raft messages to other peers.
func (p *PDRaftPeer) SetSendFunc(f func([]raftpb.Message)) { p.sendFunc = f }

// SetApplyFunc sets the function used to apply committed PDCommands.
func (p *PDRaftPeer) SetApplyFunc(f func(PDCommand) ([]byte, error)) { p.applyFunc = f }

// SetApplySnapshotFunc sets the function used to apply a received snapshot.
func (p *PDRaftPeer) SetApplySnapshotFunc(f func([]byte) error) { p.applySnapshotFunc = f }

// SetLeaderChangeFunc sets a callback that fires when the local leader status
// changes. The callback receives true if this node became the leader, false
// otherwise. This is used to reset buffered allocators on leader change.
func (p *PDRaftPeer) SetLeaderChangeFunc(f func(isLeader bool)) { p.leaderChangeFunc = f }

// WireTransport sets up p.sendFunc to route outbound Raft messages via the
// given PDTransport. Messages addressed to the local node are delivered
// directly to the peer's own mailbox (avoiding a network round-trip).
// Errors from transport.Send are logged and skipped; Raft handles
// retransmission automatically.
func (p *PDRaftPeer) WireTransport(transport *PDTransport) {
	p.sendFunc = func(msgs []raftpb.Message) {
		for _, msg := range msgs {
			if msg.To == p.nodeID {
				// Local delivery: bypass the network.
				msgCopy := msg
				select {
				case p.Mailbox <- PDRaftMsg{
					Type: PDRaftMsgTypeRaftMessage,
					Data: &msgCopy,
				}:
				default:
					slog.Warn("pd: local mailbox full, dropping self-message",
						"node", p.nodeID, "type", msg.Type)
				}
				continue
			}

			if err := transport.Send(msg.To, msg); err != nil {
				slog.Warn("pd: failed to send raft message",
					"node", p.nodeID, "to", msg.To,
					"type", msg.Type, "err", err)
			}
		}
	}
}

// IsStopped returns whether this peer has been stopped.
func (p *PDRaftPeer) IsStopped() bool { return p.stopped.Load() }
