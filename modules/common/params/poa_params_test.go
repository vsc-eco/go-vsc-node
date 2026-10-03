package params_test

import (
	"testing"

	"vsc-node/modules/common/params"
)

// The POA params are consensus-critical: an admit-vote window or an exit-halt
// that resolves differently on two nodes means two different seat sets and two
// different ledger outcomes. These tests pin the fallback behaviour so an unset
// (zero) field can never silently mean "no window" or "no halt".

func TestEffectivePoaAdmitVoteWindowFallsBackNotToZero(t *testing.T) {
	var unset params.ConsensusParams
	if got := unset.EffectivePoaAdmitVoteWindow(); got != params.DefaultPoaAdmitVoteWindowBlocks {
		t.Fatalf("unset admit-vote window = %d, want default %d", got, params.DefaultPoaAdmitVoteWindowBlocks)
	}
	if got := unset.EffectivePoaAdmitVoteWindow(); got == 0 {
		t.Fatal("unset admit-vote window resolved to 0 — a proposal would expire in the block it opened")
	}
	pinned := params.ConsensusParams{PoaAdmitVoteWindowBlocks: 42}
	if got := pinned.EffectivePoaAdmitVoteWindow(); got != 42 {
		t.Fatalf("pinned admit-vote window = %d, want 42", got)
	}
}

func TestEffectivePoaExitHaltFallsBackNotToZero(t *testing.T) {
	var unset params.ConsensusParams
	if got := unset.EffectivePoaExitHalt(); got != params.DefaultPoaExitHaltBlocks {
		t.Fatalf("unset exit halt = %d, want default %d", got, params.DefaultPoaExitHaltBlocks)
	}
	if got := unset.EffectivePoaExitHalt(); got == 0 {
		t.Fatal("unset exit halt resolved to 0 — collateral would be withdrawable the instant a seat exits, which is the escape the halt exists to close")
	}
	pinned := params.ConsensusParams{PoaExitHaltBlocks: 7}
	if got := pinned.EffectivePoaExitHalt(); got != 7 {
		t.Fatalf("pinned exit halt = %d, want 7", got)
	}
}

// A cap of 0 would mean "admit nobody, ever" — a silent, permanent liveness
// stop on admission. Both unset and a negative (mis-signed/mis-parsed) value
// must resolve to the safe default instead.
func TestEffectivePoaMaxNewMembersNeverZero(t *testing.T) {
	for _, in := range []int{0, -1, -1000} {
		cp := params.ConsensusParams{PoaMaxNewMembersPerElection: in}
		got := cp.EffectivePoaMaxNewMembers()
		if got != params.DefaultPoaMaxNewMembersPerElection {
			t.Fatalf("cap %d resolved to %d, want default %d", in, got, params.DefaultPoaMaxNewMembersPerElection)
		}
		if got <= 0 {
			t.Fatalf("cap %d resolved to %d — a non-positive cap wedges admission permanently", in, got)
		}
	}
	pinned := params.ConsensusParams{PoaMaxNewMembersPerElection: 3}
	if got := pinned.EffectivePoaMaxNewMembers(); got != 3 {
		t.Fatalf("pinned cap = %d, want 3", got)
	}
}

// Flat seat-weight is the mechanism that turns "2/3 of stake" into "2/3 of
// seats". If the weight were ever 0 the whole election would carry zero total
// weight and every ceil(2W/3) threshold would collapse to 0 — i.e. any single
// signature would satisfy quorum.
func TestPoaSeatWeightIsPositiveAndExact(t *testing.T) {
	if params.PoaSeatWeight == 0 {
		t.Fatal("PoaSeatWeight is 0 — every weight threshold would collapse to 0 and one signature would meet quorum")
	}
	// With 20 seats at weight 1, ceil(2W/3) must land on 14 — the number the
	// POA design is stated in terms of.
	const seats = 20
	totalWeight := params.PoaSeatWeight * seats
	if got := (2*totalWeight + 2) / 3; got != 14 {
		t.Fatalf("ceil(2/3) over 20 seats = %d, want 14", got)
	}
}
