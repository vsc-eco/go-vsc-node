package state_engine

import (
	"testing"

	"vsc-node/modules/db/vsc/elections"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
)

func TestRecordEvidenceAndShouldSlash_ThresholdAndDedup(t *testing.T) {
	se := &StateEngine{
		safetyEvidenceSeen:    make(map[string]uint64),
		safetyEvidenceHeights: make(map[string][]uint64),
	}
	acct := "alice"
	kind := safetyslash.EvidenceTSSEquivocation
	if se.recordEvidenceAndShouldSlash(acct, kind, "e1", 100) {
		t.Fatal("first thresholded evidence should not slash yet")
	}
	if se.recordEvidenceAndShouldSlash(acct, kind, "e1", 100) {
		t.Fatal("duplicate evidence id must not slash")
	}
	if !se.recordEvidenceAndShouldSlash(acct, kind, "e2", 120) {
		t.Fatal("second unique evidence should meet threshold and slash")
	}
}

func TestRecordEvidenceAndShouldSlash_ImmediateKind(t *testing.T) {
	se := &StateEngine{
		safetyEvidenceSeen:    make(map[string]uint64),
		safetyEvidenceHeights: make(map[string][]uint64),
	}
	if !se.recordEvidenceAndShouldSlash("alice", safetyslash.EvidenceVSCInvalidBlockProposal, "tx1", 100) {
		t.Fatal("immediate evidence kind should slash on first proof")
	}
}

func TestBlamedAccountsFromBitSet(t *testing.T) {
	members := []elections.ElectionMember{
		{Account: "alice"},
		{Account: "bob"},
		{Account: "carol"},
	}
	// bits 0 and 2 set => alice, carol
	got := blamedAccountsFromBitSet("05", members)
	if len(got) != 2 || got[0] != "hive:alice" || got[1] != "hive:carol" {
		t.Fatalf("unexpected blamed accounts: %#v", got)
	}
}

func TestRecordEvidenceAndShouldSlash_ThresholdWindowExpiry(t *testing.T) {
	se := &StateEngine{
		safetyEvidenceSeen:    make(map[string]uint64),
		safetyEvidenceHeights: make(map[string][]uint64),
	}
	acct := "alice"
	kind := safetyslash.EvidenceTSSEquivocation
	if se.recordEvidenceAndShouldSlash(acct, kind, "e1", 100) {
		t.Fatal("first evidence should not slash")
	}
	// Outside window => should behave as first evidence again.
	if se.recordEvidenceAndShouldSlash(acct, kind, "e2", 1201) {
		t.Fatal("evidence outside window should not satisfy threshold")
	}
	// Second within current window triggers slash.
	if !se.recordEvidenceAndShouldSlash(acct, kind, "e3", 1202) {
		t.Fatal("second evidence in new window should satisfy threshold")
	}
}

