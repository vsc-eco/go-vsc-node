package governance

import "testing"

func members(pairs ...any) []Member {
	out := make([]Member, 0, len(pairs)/2)
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, Member{Account: pairs[i].(string), Weight: uint64(pairs[i+1].(int))})
	}
	return out
}

func voterSet(accts ...string) map[string]bool {
	m := make(map[string]bool, len(accts))
	for _, a := range accts {
		m[NormalizeAccount(a)] = true
	}
	return m
}

func TestNormalizeAccount(t *testing.T) {
	cases := map[string]string{
		"hive:Mengao.VSC": "mengao.vsc",
		"  milo.vsc  ":    "milo.vsc",
		"BOB":             "bob",
		"hive:alice":      "alice",
	}
	for in, want := range cases {
		if got := NormalizeAccount(in); got != want {
			t.Errorf("NormalizeAccount(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestThresholdAndTally pins the supermajority math and beneficiary exclusion.
// Electorate of 4 equal-weight members; the beneficiary is excluded from both
// numerator and denominator, so the effective electorate is 3 and the threshold
// is ceil(2*3/3)=2.
func TestThresholdAndTally(t *testing.T) {
	e := members("a", 1, "b", 1, "c", 1, "victim", 1)
	ben := "victim"

	if got := EffectiveWeight(e, ben); got != 3 {
		t.Fatalf("EffectiveWeight = %d, want 3 (beneficiary excluded)", got)
	}
	if got := RequiredWeight(e, ben); got != 2 {
		t.Fatalf("RequiredWeight = %d, want 2 (ceil(2*3/3))", got)
	}

	// One non-beneficiary vote: below threshold.
	if IsApproved(e, ben, voterSet("a")) {
		t.Error("1 of 3 should not approve")
	}
	// Two non-beneficiary votes: meets threshold.
	if !IsApproved(e, ben, voterSet("a", "b")) {
		t.Error("2 of 3 should approve")
	}
	// The beneficiary's own vote must not count toward the tally.
	if Tally(e, ben, voterSet("victim")) != 0 {
		t.Error("beneficiary self-vote must contribute 0")
	}
	if IsApproved(e, ben, voterSet("victim", "a")) {
		t.Error("beneficiary + 1 other (=1 counting vote) should not approve")
	}
}

// TestWeightedThreshold checks weights, not just counts, drive the tally.
func TestWeightedThreshold(t *testing.T) {
	// Total non-beneficiary weight = 10+5+3 = 18; required = ceil(36/3)=12.
	e := members("big", 10, "mid", 5, "small", 3, "victim", 100)
	ben := "victim"
	if got := RequiredWeight(e, ben); got != 12 {
		t.Fatalf("RequiredWeight = %d, want 12", got)
	}
	if !IsApproved(e, ben, voterSet("big", "mid")) { // 15 >= 12
		t.Error("big+mid (15) should approve")
	}
	if IsApproved(e, ben, voterSet("big", "small")) { // 13 >= 12 -> approve
		// 13 >= 12 actually approves; assert that
	} else {
		t.Error("big+small (13) should approve")
	}
	if IsApproved(e, ben, voterSet("mid", "small")) { // 8 < 12
		t.Error("mid+small (8) should not approve")
	}
	// The huge-weight beneficiary cannot tip its own payout.
	if IsApproved(e, ben, voterSet("victim")) {
		t.Error("beneficiary weight must be excluded entirely")
	}
}

// TestDegenerateElectorate guards against auto-approval when the effective
// (beneficiary-excluded) electorate is empty.
func TestDegenerateElectorate(t *testing.T) {
	if RequiredWeight(members("victim", 5), "victim") != 0 {
		t.Error("all-beneficiary electorate should have 0 required weight")
	}
	if IsApproved(members("victim", 5), "victim", voterSet("victim")) {
		t.Error("empty effective electorate must never auto-approve")
	}
	if IsApproved(nil, "victim", voterSet()) {
		t.Error("nil electorate must never auto-approve")
	}
}

// TestEffectiveExpiry pins the simple-cap rule: min(creation+expiry, maturity),
// with maturity==0 meaning no cap.
func TestEffectiveExpiry(t *testing.T) {
	const expiry = 86400 // 3d
	// No maturity cap (reserve_payout): pure creation+expiry.
	if got := EffectiveExpiry(1000, expiry, 0); got != 1000+expiry {
		t.Errorf("no-cap expiry = %d, want %d", got, 1000+expiry)
	}
	// Maturity far in the future: expiry wins.
	if got := EffectiveExpiry(1000, expiry, 10_000_000); got != 1000+expiry {
		t.Errorf("distant-maturity expiry = %d, want %d", got, 1000+expiry)
	}
	// Maturity sooner than expiry: maturity caps the window.
	if got := EffectiveExpiry(1000, expiry, 5000); got != 5000 {
		t.Errorf("near-maturity expiry = %d, want 5000", got)
	}

	// IsExpired boundary is inclusive.
	if !IsExpired(5000, 1000, expiry, 5000) {
		t.Error("at maturity height the proposal must be expired")
	}
	if IsExpired(4999, 1000, expiry, 5000) {
		t.Error("just before maturity the proposal must be live")
	}
}
