package consensusversion

import "testing"

// KEY-SPAM-1 (18j). The allowlist changes which transactions SUCCEED, so an
// un-gated rollout would have upgraded nodes rejecting a create_key that
// un-upgraded nodes accept — a fork. It must therefore be off for every
// version below 0.13.0 (mainnet reports 0.12) and on from it.
func TestTssKeyAllowlistActivatesAtThirteen(t *testing.T) {
	for _, tc := range []struct {
		active Version
		want   bool
	}{
		{Version{}, false},
		{Version{Major: 0, Consensus: 3}, false},
		{V0_11_0, false},
		{V0_12_0, false}, // what mainnet runs today: must be inert
		{V0_13_0, true},
		{Version{Major: 0, Consensus: 14}, true},
	} {
		if got := TssKeyAllowlistActive(tc.active); got != tc.want {
			t.Errorf("TssKeyAllowlistActive(%s) = %v, want %v", tc.active.Format(), got, tc.want)
		}
	}
}
