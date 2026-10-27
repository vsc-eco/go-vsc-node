package consensusversion

import "testing"

// The TSS-party rules are inert below 0.12.0 (mainnet runs 0.3.0, testnet
// 0.11.0) and all in force from it, so history re-executes unchanged and every
// node switches at the same election.
func TestTssPartyGatesActivateAtTwelveTogether(t *testing.T) {
	gates := map[string]func(Version) bool{
		"TssAccuseYieldsToFloorActive": TssAccuseYieldsToFloorActive,
		"PeerIdPoPActive":              PeerIdPoPActive,
		"PendingKeyReshareActive":      PendingKeyReshareActive,
		"HivePrefixRecipientActive":    HivePrefixRecipientActive,
	}
	for name, gate := range gates {
		for _, tc := range []struct {
			active Version
			want   bool
		}{
			{Version{}, false},
			{Version{Major: 0, Consensus: 3}, false},
			{V0_11_0, false},
			{V0_12_0, true},
			{Version{Major: 0, Consensus: 13}, true},
		} {
			if got := gate(tc.active); got != tc.want {
				t.Errorf("%s(%s) = %v, want %v", name, tc.active.Format(), got, tc.want)
			}
		}
	}
}
