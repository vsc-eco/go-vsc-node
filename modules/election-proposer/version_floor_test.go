package election_proposer

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/db/vsc/consensus_state"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/witnesses"
)

// version_floor_test.go covers resolveVersionFloor — the consensus-version floor
// advance the election proposer applies. This is the guard that actually moves the
// floor on a proposal, which previously had NO test coverage.

func v(major, consensus uint64) consensusversion.Version {
	return consensusversion.Version{Major: major, Consensus: consensus}
}

// wlist builds a committee where the first `ready` witnesses announce `target` and
// the rest announce `floorV` (below target), all weight 1.
func wlist(n, ready int, target, floorV consensusversion.Version) ([]witnesses.Witness, map[string]uint64) {
	ws := make([]witnesses.Witness, 0, n)
	wm := map[string]uint64{}
	for i := 0; i < n; i++ {
		acct := string(rune('a' + i))
		ver := floorV
		if i < ready {
			ver = target
		}
		ws = append(ws, witnesses.Witness{Account: acct, VersionMajor: ver.Major, ProtocolVersion: ver.Consensus})
		wm[acct] = 1
	}
	return ws, wm
}

func prop(major, consensus, activationEpoch, expiryEpoch uint64, proposer string) consensus_state.VersionProposal {
	return consensus_state.VersionProposal{
		TargetMajor: major, TargetConsensus: consensus,
		ActivationEpoch: activationEpoch, ExpiryEpoch: expiryEpoch, Proposer: proposer,
	}
}

const num, den = int64(4), int64(5) // 80%

// TestResolveVersionFloor_RisesAtThreshold: 4/5 ready exactly meets 80% → adopt.
func TestResolveVersionFloor_RisesAtThreshold(t *testing.T) {
	floor := v(0, 0)
	target := v(0, 3)
	ws, wm := wlist(5, 4, target, floor)
	got := resolveVersionFloor(floor, 2, 100, nil,
		[]consensus_state.VersionProposal{prop(0, 3, 1, 0, "a")}, ws, wm, nil, num, den)
	if got.Cmp(target) != 0 {
		t.Fatalf("floor = %s, want %s (4/5 = 80%% ready meets threshold)", got.Format(), target.Format())
	}
}

// TestResolveVersionFloor_StaysBelowThreshold: 3/5 ready (60%) → no advance.
func TestResolveVersionFloor_StaysBelowThreshold(t *testing.T) {
	floor := v(0, 0)
	target := v(0, 3)
	ws, wm := wlist(5, 3, target, floor)
	got := resolveVersionFloor(floor, 2, 100, nil,
		[]consensus_state.VersionProposal{prop(0, 3, 1, 0, "a")}, ws, wm, nil, num, den)
	if got.Cmp(floor) != 0 {
		t.Fatalf("floor = %s, want unchanged %s (3/5 < 80%%)", got.Format(), floor.Format())
	}
}

// TestResolveVersionFloor_ForcedBypassesReadiness: a recovery override adopts even
// with zero stake-readiness.
func TestResolveVersionFloor_ForcedBypassesReadiness(t *testing.T) {
	floor := v(0, 0)
	ws, wm := wlist(5, 0, v(0, 5), floor) // nobody announces anything above floor
	forced := &consensus_state.VersionProposal{TargetMajor: 0, TargetConsensus: 5, ActivationEpoch: 1, Forced: true}
	got := resolveVersionFloor(floor, 2, 100, forced, nil, ws, wm, nil, num, den)
	if got.Cmp(v(0, 5)) != 0 {
		t.Fatalf("floor = %s, want 0.5 (forced bypasses readiness)", got.Format())
	}
}

// TestResolveVersionFloor_HighestQualifyingWins: two ready candidates → the higher.
func TestResolveVersionFloor_HighestQualifyingWins(t *testing.T) {
	floor := v(0, 0)
	ws, wm := wlist(5, 5, v(0, 4), floor) // all 5 announce 0.4 (so ready for 0.3 and 0.4)
	props := []consensus_state.VersionProposal{prop(0, 3, 1, 0, "a"), prop(0, 4, 1, 0, "b")}
	got := resolveVersionFloor(floor, 2, 100, nil, props, ws, wm, nil, num, den)
	if got.Cmp(v(0, 4)) != 0 {
		t.Fatalf("floor = %s, want 0.4 (highest qualifying candidate)", got.Format())
	}
}

// TestResolveVersionFloor_PoisonIgnored: a junk high target nobody runs cannot block
// a legitimate lower one that is ready.
func TestResolveVersionFloor_PoisonIgnored(t *testing.T) {
	floor := v(0, 0)
	ws, wm := wlist(5, 5, v(0, 3), floor) // all ready for 0.3, none for 99.0
	props := []consensus_state.VersionProposal{prop(0, 3, 1, 0, "a"), prop(99, 0, 1, 0, "griefer")}
	got := resolveVersionFloor(floor, 2, 100, nil, props, ws, wm, nil, num, den)
	if got.Cmp(v(0, 3)) != 0 {
		t.Fatalf("floor = %s, want 0.3 (poison 99.0 ignored, real 0.3 adopted)", got.Format())
	}
}

// TestResolveVersionFloor_PrevCommitteeQuorumBlocks: the new committee is fully
// ready, but the OUTGOING committee is below TSS-reshare quorum at target → blocked.
func TestResolveVersionFloor_PrevCommitteeQuorumBlocks(t *testing.T) {
	floor := v(0, 0)
	target := v(0, 3)
	ws, wm := wlist(3, 3, target, floor) // new committee 100% ready
	// Previous committee: 3 members, but only 1 is a current candidate on target
	// (the other two churned out) → prevReady=1 < quorum ceil(2*3/3)=2 → blocked.
	prev := &elections.ElectionResult{ElectionDataInfo: elections.ElectionDataInfo{
		Members: []elections.ElectionMember{{Account: "a"}, {Account: "x"}, {Account: "y"}},
	}}
	got := resolveVersionFloor(floor, 2, 100, nil,
		[]consensus_state.VersionProposal{prop(0, 3, 1, 0, "a")}, ws, wm, prev, num, den)
	if got.Cmp(floor) != 0 {
		t.Fatalf("floor = %s, want unchanged (outgoing committee below reshare quorum)", got.Format())
	}
}

// TestResolveVersionFloor_SubFloorAndExpiredAndInactiveSkipped: candidates at/below
// floor, past expiry, or before activation epoch are not adopted.
func TestResolveVersionFloor_SubFloorAndExpiredAndInactiveSkipped(t *testing.T) {
	floor := v(0, 2)
	ws, wm := wlist(5, 5, v(0, 9), floor) // everyone ready for anything
	props := []consensus_state.VersionProposal{
		prop(0, 2, 1, 0, "a"),  // == floor → skip
		prop(0, 1, 1, 0, "b"),  // < floor → skip
		prop(0, 3, 1, 5, "c"),  // expired (expiry 5 <= epoch 10) → skip
		prop(0, 4, 99, 0, "d"), // not yet active (activation 99 > epoch 10) → skip
	}
	got := resolveVersionFloor(floor, 10, 100, nil, props, ws, wm, nil, num, den)
	if got.Cmp(floor) != 0 {
		t.Fatalf("floor = %s, want unchanged %s (all candidates ineligible)", got.Format(), floor.Format())
	}
}
