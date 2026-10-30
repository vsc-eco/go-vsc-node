package consensusversion

import "testing"

// The pre-mainnet fixes are inert below 0.10.0 (mainnet runs 0.3.0, testnet
// 0.9.0) and all in force from it, so history re-executes unchanged and every
// node switches at the same election.
func TestPremainnetFixGatesActivateAtTenTogether(t *testing.T) {
	gates := map[string]func(Version) bool{
		"Sp1WorkPricingActive":      Sp1WorkPricingActive,
		"ApplySkipsHandledTxActive": ApplySkipsHandledTxActive,
	}
	for name, gate := range gates {
		for _, tc := range []struct {
			active Version
			want   bool
		}{
			{Version{}, false},
			{Version{Major: 0, Consensus: 3}, false},
			{Version{Major: 0, Consensus: 9}, false},
			{V0_10_0, true},
			{Version{Major: 0, Consensus: 11}, true},
		} {
			if got := gate(tc.active); got != tc.want {
				t.Errorf("%s(%s) = %v, want %v", name, tc.active.Format(), got, tc.want)
			}
		}
	}
}
