package state_engine

import (
	"testing"

	"vsc-node/modules/db/vsc/elections"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
)

// TestRecordEvidenceAndShouldSlash_ImmediateAndDedup covers the only surface
// the principal-slash policy currently exposes: every wired kind is immediate
// (UsesThreshold=false), but duplicate evidence ids must never re-slash so
// detectors are safe to re-enter on replay.
func TestRecordEvidenceAndShouldSlash_ImmediateAndDedup(t *testing.T) {
	se := &StateEngine{
		safetyEvidenceSeen:    make(map[string]uint64),
		safetyEvidenceHeights: make(map[string][]uint64),
	}
	if !se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCDoubleBlockSign, "ev-1", 100) {
		t.Fatal("immediate evidence kind should slash on first proof")
	}
	if se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCDoubleBlockSign, "ev-1", 100) {
		t.Fatal("duplicate evidence id at same height must not re-slash")
	}
	if !se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCDoubleBlockSign, "ev-2", 105) {
		t.Fatal("distinct evidence id should slash again")
	}
	if !se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCInvalidBlockProposal, "ev-1", 110) {
		t.Fatal("different kind with same evidence id should slash independently")
	}
}

// TestRecordEvidenceAndShouldSlash_RejectsBlankInputs guards the early-exit
// validation in the policy entrypoint so callers cannot accidentally slash
// with an empty account or kind string.
func TestRecordEvidenceAndShouldSlash_RejectsBlankInputs(t *testing.T) {
	se := &StateEngine{
		safetyEvidenceSeen:    make(map[string]uint64),
		safetyEvidenceHeights: make(map[string][]uint64),
	}
	if se.recordEvidenceAndShouldSlash("", safetyslash.EvidenceVSCDoubleBlockSign, "ev", 100) {
		t.Fatal("blank account must not slash")
	}
	if se.recordEvidenceAndShouldSlash("alice", "", "ev", 100) {
		t.Fatal("blank kind must not slash")
	}
}

// TestBlamedAccountsFromBitSet exercises the blame-bitset parser. The function
// no longer drives a principal slash (TSS blame is liveness-only), but the
// helper stays so future reward-reduction wiring can reuse it deterministically.
func TestBlamedAccountsFromBitSet(t *testing.T) {
	members := []elections.ElectionMember{
		{Account: "alice"},
		{Account: "bob"},
		{Account: "carol"},
	}
	got := blamedAccountsFromBitSet("05", members)
	if len(got) != 2 || got[0] != "hive:alice" || got[1] != "hive:carol" {
		t.Fatalf("unexpected blamed accounts: %#v", got)
	}
}
