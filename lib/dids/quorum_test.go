package dids

import "testing"

// B13. A committee holding the same BLS key at two seats produces an aggregate
// that VERIFIES: aggregateSignaturesFrom walks the keyset by index while
// signatures are keyed by DID, so one honest signature matches the duplicated DID
// at both indices, and the aggregate becomes 2S against a keyset summing to 2P —
// which pairs correctly. The attacker needs neither the copied private key nor a
// second signature, and the honest validator never knows.
//
// So the duplicate has to be caught in the weight fold, and these tests pin the
// three properties that makes it safe to do there.
func TestFoldSignedWeightCollapsesDuplicateSeats(t *testing.T) {
	// "b" is seated twice. The attacker chose the second account name, and gave
	// that seat a large self-staked weight.
	memberKeys := []string{"did:honest:a", "did:honest:b", "did:honest:c", "did:honest:b"}
	weights := []uint64{10, 10, 10, 70}

	// Only the honest members sign. The duplicated DID appears once in the
	// verified included set, because that is what the aggregate reports.
	included := []BlsDID{"did:honest:a", "did:honest:b"}

	signed, total, ok := FoldSignedWeight(memberKeys, weights, included)
	if !ok {
		t.Fatal("fold should produce a verdict for a well-formed election")
	}

	// The phantom seat is gone from BOTH sides of the ratio.
	if total != 30 {
		t.Errorf("total = %d, want 30: the duplicate seat cannot sign independently, "+
			"so it must not raise the bar honest signers have to clear", total)
	}
	// And its weight was never credited. Under the old last-write-wins map, the
	// single honest signature from "b" would have been credited the attacker's 70.
	if signed != 20 {
		t.Errorf("signed = %d, want 20: a duplicated key is ONE signer and must be "+
			"credited ONE seat's weight, the first occurrence's", signed)
	}
}

// The tie-break is first-occurrence, which is the lexicographically-first account
// because members arrive account-sorted. Last-wins would hand the choice to the
// attacker, who need only pick a name sorting after their victim's — that was the
// exact defect in the previous implementation, whose comment claimed "duplicates
// counted once" while the map overwrite made the credited weight attacker-chosen.
func TestFoldSignedWeightTieBreakIsNotAttackerChosen(t *testing.T) {
	victimFirst := []string{"did:x", "did:x"}

	// Whichever order the attacker's inflated weight is placed in, the credited
	// weight is the first seat's.
	for _, tc := range []struct {
		name    string
		weights []uint64
		want    uint64
	}{
		{"attacker seat second", []uint64{10, 90}, 10},
		{"attacker seat first", []uint64{90, 10}, 90},
	} {
		signed, total, ok := FoldSignedWeight(victimFirst, tc.weights, []BlsDID{"did:x"})
		if !ok {
			t.Fatalf("%s: fold should produce a verdict", tc.name)
		}
		if signed != tc.want || total != tc.want {
			t.Errorf("%s: signed/total = %d/%d, want %d/%d (first occurrence wins)",
				tc.name, signed, total, tc.want, tc.want)
		}
	}
}

// A duplicate must never stop the network. Refusing to produce a verdict would
// turn this into a chain-halt: the same fold gates TSS commitments and drives
// BlockInvalid, which SLASHES the proposer — so anyone able to seat one duplicate
// key could stall the chain and get honest proposers punished.
func TestFoldSignedWeightDoesNotFailClosedOnADuplicate(t *testing.T) {
	_, _, ok := FoldSignedWeight([]string{"did:x", "did:x"}, []uint64{10, 10}, []BlsDID{"did:x"})
	if !ok {
		t.Error("a duplicate seat must be dropped, not turned into a refusal to " +
			"produce a verdict — that is a liveness kill switch, not a fix")
	}
}

// Inputs that cannot yield a meaningful verdict must not yield a favourable one.
func TestFoldSignedWeightFailsClosedOnMalformedInput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		keys    []string
		weights []uint64
	}{
		{"empty committee", nil, nil},
		{"length mismatch", []string{"did:a", "did:b"}, []uint64{1}},
		{"zero total weight", []string{"did:a"}, []uint64{0}},
	} {
		if _, _, ok := FoldSignedWeight(tc.keys, tc.weights, []BlsDID{"did:a"}); ok {
			t.Errorf("%s: must not produce a verdict", tc.name)
		}
		if QuorumMet(tc.keys, tc.weights, []BlsDID{"did:a"}) {
			t.Errorf("%s: must not report quorum", tc.name)
		}
	}
}

// An inflated bitset cannot manufacture weight: unknown DIDs count zero and a
// repeated DID counts once.
func TestFoldSignedWeightIgnoresUnknownAndRepeatedSigners(t *testing.T) {
	keys := []string{"did:a", "did:b"}
	weights := []uint64{10, 10}

	signed, _, ok := FoldSignedWeight(keys, weights, []BlsDID{"did:a", "did:a", "did:stranger"})
	if !ok {
		t.Fatal("fold should produce a verdict")
	}
	if signed != 10 {
		t.Errorf("signed = %d, want 10: a repeated DID counts once and an unknown one counts zero", signed)
	}
}

// GV-L9: the overflow-safe form must agree with the naive product form across the
// whole reachable range, and must NOT wrap above it.
func TestTwoThirdsMet(t *testing.T) {
	for _, tc := range []struct {
		signed, total uint64
		want          bool
	}{
		{0, 0, false},  // no committee
		{2, 3, true},   // exactly 2/3
		{1, 3, false},  // below
		{3, 3, true},   // unanimous
		{66, 100, false},
		{67, 100, true},
		// Above MaxUint64/2 the naive signed*3 >= total*2 wraps and would report
		// quorum for a ZERO-weight commitment. This form must not.
		{0, 1 << 63, false},
	} {
		if got := TwoThirdsMet(tc.signed, tc.total); got != tc.want {
			t.Errorf("TwoThirdsMet(%d, %d) = %v, want %v", tc.signed, tc.total, got, tc.want)
		}
	}
}

// TestLeaderSigsComplete_StopsOnlyWhenTheBlockCanPass: wherever the leader
// stops, the weight it has must already clear the strict 2/3 bar the block is
// then validated against (TxProposeBlock.ValidateDetailed). The 90% term alone
// stops at 2 of 3, which that check rejects — the L2-STALL-1 regression that
// left every 3-seat committee unable to broadcast with all three online.
func TestLeaderSigsComplete_StopsOnlyWhenTheBlockCanPass(t *testing.T) {
	for total := uint64(1); total <= 64; total++ {
		stop := uint64(0)
		for !LeaderSigsComplete(stop, total, total) && stop <= total {
			stop++
		}
		if stop > total {
			t.Fatalf("total %d: the leader never stops even with every member signed", total)
		}
		if !(stop > total*2/3) {
			t.Fatalf("total %d: the leader stops at %d, which fails the block's 2/3 check (needs > %d)",
				total, stop, total*2/3)
		}
	}
}

// TestLeaderSigsComplete_SixteenSeatsStopAtFourteen pins the number
// REWARD-ATTEST-1 turns on. The live mainnet committee is 16 POA seats of
// weight 1, so the leader broadcasts the moment 14 signatures are in and never
// waits for the other two. Pendulum settlement replays this bar to decide
// whether a block's absentees are evidence of anything, so the figure is now
// consensus-relevant: changing it belongs behind a new version line, and this
// assertion is here to make a silent change loud.
func TestLeaderSigsComplete_SixteenSeatsStopAtFourteen(t *testing.T) {
	const total = uint64(16)
	for signed := uint64(0); signed < 14; signed++ {
		if LeaderSigsComplete(signed, total, total) {
			t.Fatalf("16 seats: leader stopped at %d signatures, want it to keep waiting until 14", signed)
		}
	}
	for signed := uint64(14); signed <= total; signed++ {
		if !LeaderSigsComplete(signed, total, total) {
			t.Fatalf("16 seats: leader still waiting at %d signatures, want it stopped from 14 on", signed)
		}
	}
}

// TestLeaderSigsComplete_ZeroTotalIsNeverComplete: an election whose folded
// weight is zero (FoldSignedWeight's !ok case) must never read as complete.
// For the leader that is unreachable anyway — no weight means no signature can
// be credited — but the settlement scorer asks the same question about stored
// elections, and "complete" there means "stop docking".
func TestLeaderSigsComplete_ZeroTotalIsNeverComplete(t *testing.T) {
	if LeaderSigsComplete(0, 0, 0) || LeaderSigsComplete(5, 0, 6) {
		t.Fatal("a zero folded total must never report complete")
	}
}
