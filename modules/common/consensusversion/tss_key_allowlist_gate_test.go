package consensusversion

import "testing"

// KEY-SPAM-1 (18j). The allowlist changes which transactions SUCCEED, so an
// un-gated rollout would have upgraded nodes rejecting a create_key that
// un-upgraded nodes accept — a fork.
//
// It activates at 0.14.0, NOT 0.13.0 where it was first pinned: the testnet has
// already activated 0.13 (epoch 1500), and a rule added to a line a chain is
// already on has no rollout window at all. 0.13 must therefore read FALSE here,
// and that row is the whole point of this test — it is what fails if anyone
// moves either gate back onto the EVM line.
func TestTssKeyAllowlistActivatesAtFourteen(t *testing.T) {
	for _, tc := range []struct {
		active Version
		want   bool
	}{
		{Version{}, false},
		{Version{Major: 0, Consensus: 3}, false},
		{V0_11_0, false},
		{V0_12_0, false}, // what mainnet runs today: must be inert
		{V0_13_0, false}, // what the TESTNET runs today: must be inert
		{V0_14_0, true},
		{Version{Major: 0, Consensus: 15}, true},
	} {
		if got := TssKeyAllowlistActive(tc.active); got != tc.want {
			t.Errorf("TssKeyAllowlistActive(%s) = %v, want %v", tc.active.Format(), got, tc.want)
		}
		// 18an rides the same line: both are pre-EVM TSS hardening and must
		// activate together, so a mixed fleet cannot disagree about either.
		if got := TssPerKeyFamilyBanActive(tc.active); got != tc.want {
			t.Errorf("TssPerKeyFamilyBanActive(%s) = %v, want %v", tc.active.Format(), got, tc.want)
		}
	}
}
