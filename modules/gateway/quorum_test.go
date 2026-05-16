package gateway

import "testing"

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
