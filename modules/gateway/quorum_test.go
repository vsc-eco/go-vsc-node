package gateway

import (
	"fmt"
	"slices"
	"testing"
)

// review2 HIGH #29 — the gateway multisig owner-auth weight_threshold was set
// as int(totalWeight * 2 / 3), i.e. floor(2N/3). For 10 keys that is 6, but a
// 2/3 supermajority must require ceil(2N/3) = 7. Floor makes the multisig
// strictly weaker than 2/3 (6-of-10 can move funds).
func TestGatewayWeightThreshold_CeilOfTwoThirds(t *testing.T) {
	cases := []struct {
		total int
		want  int
	}{
		{0, 0},  // guard
		{1, 1},  // ceil(0.67)
		{3, 2},  // 2N/3 exact
		{6, 4},  // exact
		{7, 5},  // ceil(4.67) — floor would give 4
		{8, 6},  // ceil(5.33) — floor would give 5
		{9, 6},  // exact
		{10, 7}, // ceil(6.67) — the reported case; floor gave 6
		{19, 13},
		// Summed stake-proportional weights (no longer a key count) — the
		// argument is now Σ assigned weights, which is ~GATEWAY_WEIGHT_SCALE.
		{10000, 6667},   // ceil(6666.67)
		{10039, 6693},   // ceil(6692.67) — scale + min-1 floor slack
		{120000, 80000}, // exact
		{262140, 174760},
	}
	for _, c := range cases {
		got := gatewayWeightThreshold(c.total)
		if got != c.want {
			t.Errorf("gatewayWeightThreshold(%d) = %d, want %d", c.total, got, c.want)
		}
		// Never weaker than a true 2/3 supermajority.
		if c.total > 0 && got*3 < c.total*2 {
			t.Errorf("gatewayWeightThreshold(%d)=%d is below 2/3", c.total, got)
		}
	}
}

// Item 10: under POA flat weight every gateway key has the same election
// weight. At 0.9.0 each gets weight 1, so exactly 12 of 18 signers reach the
// threshold whichever 12 sign; before, 10000/18 left 556/555 by account name
// and some 12-key sets fell short.
func TestGatewayKeyWeights_EqualStakesGetEqualWeight(t *testing.T) {
	const n = 18
	stakes := make([]uint64, n)
	accounts := make([]string, n)
	for i := range stakes {
		stakes[i] = 1
		accounts[i] = fmt.Sprintf("w%02d", i)
	}

	w := gatewayKeyWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE, true)
	total := 0
	for i, x := range w {
		if x != 1 {
			t.Fatalf("key %d weight %d, want 1", i, x)
		}
		total += x
	}
	if thr := gatewayWeightThreshold(total); thr != 12 {
		t.Fatalf("threshold %d, want 12 of 18", thr)
	}

	// Before 0.9.0: the uneven split, where the 12 lightest keys fall short.
	old := gatewayKeyWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE, false)
	sorted := slices.Clone(old)
	slices.Sort(sorted)
	oldTotal, lightest12 := 0, 0
	for i, x := range sorted {
		oldTotal += x
		if i < 12 {
			lightest12 += x
		}
	}
	if weightMeetsThreshold(uint64(lightest12), gatewayWeightThreshold(oldTotal)) {
		t.Fatalf("expected the pre-0.9.0 split to leave some 12-key sets short (lightest 12 = %d of %d)", lightest12, oldTotal)
	}
}

// Unequal stakes keep stake-proportional weights even with the gate on.
func TestGatewayKeyWeights_UnequalStakesStayProportional(t *testing.T) {
	stakes := []uint64{3, 1, 1, 1, 1, 1, 1, 1}
	accounts := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	got := gatewayKeyWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE, true)
	want := quantizeStakeWeights(stakes, accounts, GATEWAY_WEIGHT_SCALE)
	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want quantized %v", got, want)
	}
}
