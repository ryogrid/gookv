package pd

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
)

// PDCommandType identifies the type of PD state-mutation command.
type PDCommandType uint8

const (
	CmdSetBootstrapped  PDCommandType = iota + 1 // 1
	CmdPutStore                                   // 2
	CmdPutRegion                                  // 3
	CmdUpdateStoreStats                           // 4
	CmdSetStoreState                              // 5
	CmdTSOAllocate                                // 6
	CmdIDAlloc                                    // 7
	CmdUpdateGCSafePoint                          // 8
	CmdStartMove                                  // 9
	CmdAdvanceMove                                // 10
	CmdCleanupStaleMove                           // 11
	CmdCompactLog                                 // 12
)

const maxPDCommandType = CmdCompactLog

// PDCommand represents a single state-mutation that can be replicated via Raft.
// For each command type, only the relevant payload fields are populated.
type PDCommand struct {
	Type PDCommandType `json:"type"`

	// Payload fields (only the relevant field is non-nil for each type).
	Bootstrapped      *bool            `json:"bootstrapped,omitempty"`
	Store             *metapb.Store    `json:"store,omitempty"`
	Region            *metapb.Region   `json:"region,omitempty"`
	Leader            *metapb.Peer     `json:"leader,omitempty"`
	StoreStats        *pdpb.StoreStats `json:"store_stats,omitempty"`
	StoreID           uint64           `json:"store_id,omitempty"`
	StoreState        *StoreState      `json:"store_state,omitempty"`
	TSOBatchSize      int              `json:"tso_batch_size,omitempty"`
	IDBatchSize       int              `json:"id_batch_size,omitempty"`
	GCSafePoint       uint64           `json:"gc_safe_point,omitempty"`
	MoveRegionID      uint64           `json:"move_region_id,omitempty"`
	MoveSourcePeer    *metapb.Peer     `json:"move_source_peer,omitempty"`
	MoveTargetStoreID uint64           `json:"move_target_store_id,omitempty"`
	AdvanceRegion     *metapb.Region   `json:"advance_region,omitempty"`
	AdvanceLeader     *metapb.Peer     `json:"advance_leader,omitempty"`
	CleanupTimeout    time.Duration    `json:"cleanup_timeout,omitempty"`
	CompactIndex      uint64           `json:"compact_index,omitempty"`
	CompactTerm       uint64           `json:"compact_term,omitempty"`
}

// Marshal encodes a PDCommand as [1-byte type prefix] + [JSON payload].
func (c *PDCommand) Marshal() ([]byte, error) {
	if c.Type < 1 || c.Type > maxPDCommandType {
		return nil, fmt.Errorf("pd command: invalid type %d", c.Type)
	}

	payload, err := json.Marshal(c)
	if err != nil {
		return nil, fmt.Errorf("pd command: marshal payload: %w", err)
	}

	buf := make([]byte, 1+len(payload))
	buf[0] = byte(c.Type)
	copy(buf[1:], payload)
	return buf, nil
}

// UnmarshalPDCommand decodes a PDCommand from the wire format produced by Marshal.
func UnmarshalPDCommand(data []byte) (PDCommand, error) {
	if len(data) < 2 {
		return PDCommand{}, fmt.Errorf("pd command: data too short (%d bytes)", len(data))
	}

	cmdType := PDCommandType(data[0])
	if cmdType < 1 || cmdType > maxPDCommandType {
		return PDCommand{}, fmt.Errorf("pd command: unknown type %d", cmdType)
	}

	var cmd PDCommand
	if err := json.Unmarshal(data[1:], &cmd); err != nil {
		return PDCommand{}, fmt.Errorf("pd command: unmarshal payload: %w", err)
	}

	// The type prefix byte is authoritative.
	cmd.Type = cmdType
	return cmd, nil
}
