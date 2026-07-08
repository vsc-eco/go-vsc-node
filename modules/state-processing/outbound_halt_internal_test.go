package state_engine

import (
	"testing"

	"vsc-node/modules/db/vsc/consensus_state"
)

// TestOutboundHaltedAt_HeightAddressable pins the active-window semantics the
// gateway gate relies on: a halt is active iff SetHeight <= h < ExpiryHeight, so
// every cosigner resolves the identical verdict at a given tick and expiry is
// deterministic (no wall-clock, no un-halt op needed).
func TestOutboundHaltedAt_HeightAddressable(t *testing.T) {
	se := &StateEngine{}
	se.chainConsensusCache = consensus_state.ChainConsensusState{
		OutboundHalts: []consensus_state.OutboundHalt{
			{Account: "alice", SetHeight: 100, ExpiryHeight: 200},
		},
	}

	cases := []struct {
		h    uint64
		want bool
	}{
		{99, false},  // before set
		{100, true},  // inclusive lower bound
		{150, true},  // inside
		{199, true},  // last active height
		{200, false}, // exclusive upper bound
		{250, false}, // after expiry
	}
	for _, c := range cases {
		if got := se.OutboundHaltedAt(c.h); got != c.want {
			t.Fatalf("OutboundHaltedAt(%d) = %v, want %v", c.h, got, c.want)
		}
	}
}

// TestOutboundHaltedAt_Stacking verifies that multiple validators' halts stack:
// the outbound path is frozen across the UNION of active windows, so honest
// validators can extend a freeze past any single node's bounded window.
func TestOutboundHaltedAt_Stacking(t *testing.T) {
	se := &StateEngine{}
	se.chainConsensusCache = consensus_state.ChainConsensusState{
		OutboundHalts: []consensus_state.OutboundHalt{
			{Account: "alice", SetHeight: 100, ExpiryHeight: 200},
			{Account: "bob", SetHeight: 150, ExpiryHeight: 300},
		},
	}
	// Continuous coverage 100..299 via the union; 300 is clear.
	for _, h := range []uint64{100, 199, 200, 250, 299} {
		if !se.OutboundHaltedAt(h) {
			t.Fatalf("expected halted at %d (union of alice+bob windows)", h)
		}
	}
	for _, h := range []uint64{99, 300, 400} {
		if se.OutboundHaltedAt(h) {
			t.Fatalf("expected NOT halted at %d", h)
		}
	}
}

// TestOutboundHaltedAt_Deactivated confirms an early-un-halted entry (ExpiryHeight
// pulled down to the un-halt height, but retained for cooldown) is immediately
// inactive and never re-freezes.
func TestOutboundHaltedAt_Deactivated(t *testing.T) {
	se := &StateEngine{}
	se.chainConsensusCache = consensus_state.ChainConsensusState{
		OutboundHalts: []consensus_state.OutboundHalt{
			// set at 100, would have expired at 200, but un-halted at 130.
			{Account: "griefer", SetHeight: 100, ExpiryHeight: 130},
		},
	}
	for _, h := range []uint64{100, 129} {
		if !se.OutboundHaltedAt(h) {
			t.Fatalf("expected halted at %d before early un-halt", h)
		}
	}
	for _, h := range []uint64{130, 150, 199} {
		if se.OutboundHaltedAt(h) {
			t.Fatalf("expected NOT halted at %d after early un-halt", h)
		}
	}
}

// TestOutboundHaltedAt_Empty: no halts → never frozen.
func TestOutboundHaltedAt_Empty(t *testing.T) {
	se := &StateEngine{}
	if se.OutboundHaltedAt(1000) {
		t.Fatal("expected no halt on empty state")
	}
}

// TestExitsFrozenAt confirms the fix-2 gate trips on EITHER halt source: an
// active outbound halt or the heavy recovery suspension.
func TestExitsFrozenAt(t *testing.T) {
	se := &StateEngine{}

	// Via an active outbound halt.
	se.chainConsensusCache = consensus_state.ChainConsensusState{
		OutboundHalts: []consensus_state.OutboundHalt{{Account: "a", SetHeight: 10, ExpiryHeight: 20}},
	}
	if !se.ExitsFrozenAt(15) {
		t.Fatal("exits must be frozen inside an active outbound halt")
	}
	if se.ExitsFrozenAt(25) {
		t.Fatal("exits must be free once the outbound halt has expired")
	}

	// Via the recovery suspension (no outbound halt present).
	se.chainConsensusCache = consensus_state.ChainConsensusState{ProcessingSuspended: true}
	if !se.ExitsFrozenAt(25) {
		t.Fatal("exits must be frozen during a recovery suspension")
	}
}
