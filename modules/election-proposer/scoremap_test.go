package election_proposer

import (
	"reflect"
	"testing"
)

// signedBy builds n blocks each signed by the given accounts. It's a small
// helper so the window fixtures below read as "this many blocks, all signed by
// these members".
func signedBy(n int, accounts ...string) [][]string {
	blocks := make([][]string, 0, n)
	for i := 0; i < n; i++ {
		blocks = append(blocks, accounts)
	}
	return blocks
}

// TestComputeBanScores_DilutionFix is the regression for the testnet over-ban:
// a witness elected in only some of the windowed epochs must be scored against
// the blocks of the epochs it actually served, not the global total. Under the
// old global denominator this intermittent-but-faithful witness was banned;
// under the per-witness denominator it is not.
func TestComputeBanScores_DilutionFix(t *testing.T) {
	// A and B serve all 4 epochs (100 blocks each) and sign everything.
	// C is elected only in the final epoch and signs every block while it
	// serves — a perfect record over its single epoch.
	window := []epochScores{
		{members: []string{"a", "b"}, blocks: signedBy(100, "a", "b")},
		{members: []string{"a", "b"}, blocks: signedBy(100, "a", "b")},
		{members: []string{"a", "b"}, blocks: signedBy(100, "a", "b")},
		{members: []string{"a", "b", "c"}, blocks: signedBy(100, "a", "b", "c")},
	}

	sm := computeBanScores(window, 50, 75)

	// Per-witness denominators: a/b across all 4 epochs, c across only its one.
	if got := sm.Denoms["c"]; got != 100 {
		t.Fatalf("c denom = %d, want 100 (only its served epoch's blocks)", got)
	}
	if got := sm.Denoms["a"]; got != 400 {
		t.Fatalf("a denom = %d, want 400", got)
	}
	// c signed 100/100 of its opportunity → must NOT be banned.
	// (Under the old global denominator it would be: 100 < samples*0.75 = 300.)
	if len(sm.BannedNodes) != 0 {
		t.Fatalf("BannedNodes = %v, want none (intermittent witness with a perfect record)", sm.BannedNodes)
	}
	if sm.Samples != 400 {
		t.Fatalf("Samples = %d, want 400 (global block total preserved for logging)", sm.Samples)
	}
}

// TestComputeBanScores_GenuineDelinquency confirms the filter still bans a
// witness that under-signs across the epochs it actually served.
func TestComputeBanScores_GenuineDelinquency(t *testing.T) {
	// a signs every block; b is elected the whole time but signs ~40%.
	window := []epochScores{
		{members: []string{"a", "b"}, blocks: append(signedBy(40, "a", "b"), signedBy(60, "a")...)},
		{members: []string{"a", "b"}, blocks: append(signedBy(40, "a", "b"), signedBy(60, "a")...)},
	}

	sm := computeBanScores(window, 50, 75)

	if sm.Map["b"] != 80 || sm.Denoms["b"] != 200 {
		t.Fatalf("b score/denom = %d/%d, want 80/200", sm.Map["b"], sm.Denoms["b"])
	}
	// 80 < 200*0.75 = 150 → banned.
	if !reflect.DeepEqual(sm.BannedNodes, []string{"b"}) {
		t.Fatalf("BannedNodes = %v, want [b]", sm.BannedNodes)
	}
}

// TestComputeBanScores_ThinRecordFloor confirms a witness whose total
// opportunity is below the floor is never banned, even on a poor record —
// there isn't enough evidence yet.
func TestComputeBanScores_ThinRecordFloor(t *testing.T) {
	// d served one short epoch (10 blocks) and signed only 1 of them.
	window := []epochScores{
		{members: []string{"a", "d"}, blocks: append(signedBy(1, "a", "d"), signedBy(9, "a")...)},
	}

	sm := computeBanScores(window, 50, 75)

	if sm.Denoms["d"] != 10 {
		t.Fatalf("d denom = %d, want 10", sm.Denoms["d"])
	}
	// denom 10 < floor 50 → held off despite 1/10 participation.
	if len(sm.BannedNodes) != 0 {
		t.Fatalf("BannedNodes = %v, want none (below per-witness floor)", sm.BannedNodes)
	}
}

// TestComputeBanScores_ThresholdTunable confirms the threshold percent actually
// governs the verdict: the same 60%-participation witness is banned at 75% but
// cleared at 50%.
func TestComputeBanScores_ThresholdTunable(t *testing.T) {
	// keep signs everything; b is elected throughout and signs 60/100.
	window := []epochScores{
		{members: []string{"keep", "b"}, blocks: append(signedBy(60, "keep", "b"), signedBy(40, "keep")...)},
	}

	if got := computeBanScores(window, 50, 75).BannedNodes; !reflect.DeepEqual(got, []string{"b"}) {
		t.Fatalf("at 75%%: BannedNodes = %v, want [b] (60 < 75)", got)
	}
	if got := computeBanScores(window, 50, 50).BannedNodes; len(got) != 0 {
		t.Fatalf("at 50%%: BannedNodes = %v, want none (60 >= 50)", got)
	}
}

// TestComputeBanScores_Deterministic confirms BannedNodes is sorted regardless
// of map iteration order — divergent ordering across honest nodes would break
// the election CID consensus.
func TestComputeBanScores_Deterministic(t *testing.T) {
	// z, m and a are all elected throughout and all under-sign.
	window := []epochScores{
		{members: []string{"z", "m", "a", "keep"}, blocks: append(signedBy(10, "z", "m", "a", "keep"), signedBy(90, "keep")...)},
		{members: []string{"z", "m", "a", "keep"}, blocks: append(signedBy(10, "z", "m", "a", "keep"), signedBy(90, "keep")...)},
	}

	sm := computeBanScores(window, 50, 75)

	wantBanned := []string{"a", "m", "z"}
	if !reflect.DeepEqual(sm.BannedNodes, wantBanned) {
		t.Fatalf("BannedNodes = %v, want %v (sorted)", sm.BannedNodes, wantBanned)
	}
	if !reflect.DeepEqual(sm.Members, []string{"a", "keep", "m", "z"}) {
		t.Fatalf("Members = %v, want sorted [a keep m z]", sm.Members)
	}
}

// TestComputeBanScores_ReportedTestnetBan reproduces the shape of the reported
// incident (tibfox: samples=1447, score=329) at smaller scale and asserts the
// fix flips the verdict from banned to eligible when the witness served a
// single window-epoch with a strong record.
func TestComputeBanScores_ReportedTestnetBan(t *testing.T) {
	// Four ~100-block epochs; the witness is elected only in the last and
	// signs 91/100 — strong, but ~23% of the 400-block global total.
	window := []epochScores{
		{members: []string{"a", "b"}, blocks: signedBy(100, "a", "b")},
		{members: []string{"a", "b"}, blocks: signedBy(100, "a", "b")},
		{members: []string{"a", "b"}, blocks: signedBy(100, "a", "b")},
		{members: []string{"a", "b", "tibfox"}, blocks: append(signedBy(91, "a", "b", "tibfox"), signedBy(9, "a", "b")...)},
	}

	sm := computeBanScores(window, 50, 75)

	if sm.Map["tibfox"] != 91 || sm.Denoms["tibfox"] != 100 {
		t.Fatalf("tibfox score/denom = %d/%d, want 91/100", sm.Map["tibfox"], sm.Denoms["tibfox"])
	}
	// Old logic: 91 < 400*0.75 = 300 → banned. New logic: 91 >= 100*0.75 = 75 → eligible.
	for _, n := range sm.BannedNodes {
		if n == "tibfox" {
			t.Fatalf("tibfox banned under per-witness denominator; want eligible (score 91 of 100)")
		}
	}
}
