package consensusversion

import "testing"

// The key-custody rules are inert below 0.11.0 (mainnet runs 0.3.0, testnet
// 0.10.0) and all in force from it, so history re-executes unchanged and every
// node switches at the same election.
func TestKeyCustodyGatesActivateAtElevenTogether(t *testing.T) {
	gates := map[string]func(Version) bool{
		"KeyResharedFlagActive":       KeyResharedFlagActive,
		"ActiveKeyHoldersBoundActive": ActiveKeyHoldersBoundActive,
	}
	for name, gate := range gates {
		for _, tc := range []struct {
			active Version
			want   bool
		}{
			{Version{}, false},
			{Version{Major: 0, Consensus: 3}, false},
			{V0_10_0, false},
			{V0_11_0, true},
			{Version{Major: 0, Consensus: 12}, true},
		} {
			if got := gate(tc.active); got != tc.want {
				t.Errorf("%s(%s) = %v, want %v", name, tc.active.Format(), got, tc.want)
			}
		}
	}
}

// The EVM key-custody rules and the keygen-once rule are inert below 0.13.0,
// so 0.11.0 (the BTC vault release) and 0.12.0 (the TSS-PARTY batch) keep
// their behaviour, and all in force from it.
func TestEvmVaultCustodyActivatesAtThirteen(t *testing.T) {
	gates := map[string]func(Version) bool{
		"EvmVaultCustodyActive": EvmVaultCustodyActive,
		"KeygenOnceActive":      KeygenOnceActive,
	}
	for name, gate := range gates {
		for _, tc := range []struct {
			active Version
			want   bool
		}{
			{Version{}, false},
			{V0_10_0, false},
			{V0_11_0, false},
			{V0_12_0, false},
			{V0_13_0, true},
			{Version{Major: 0, Consensus: 14}, true},
		} {
			if got := gate(tc.active); got != tc.want {
				t.Errorf("%s(%s) = %v, want %v", name, tc.active.Format(), got, tc.want)
			}
		}
	}
}
