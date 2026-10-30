package state_engine

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
)

// The 0.11.0 key-custody rules follow the chain-active version on shipped
// networks, and the vault-rotation-v2 height pin on ephemeral ones (a fresh
// devnet has no floor to raise), so a pinned devnet running the new vault
// contract can still migrate.
func TestKeyCustodyInForce(t *testing.T) {
	mainnet := systemconfig.MainnetConfig().ConsensusParams()
	for _, tc := range []struct {
		name   string
		active consensusversion.Version
		want   bool
	}{
		{"mainnet today (0.3.0)", consensusversion.Version{Major: 0, Consensus: 3}, false},
		{"testnet today (0.10.0)", consensusversion.V0_10_0, false},
		{"floor at 0.11.0", consensusversion.V0_11_0, true},
	} {
		if got := KeyCustodyInForce(mainnet, 1<<40, tc.active); got != tc.want {
			t.Errorf("%s: KeyCustodyInForce = %v, want %v", tc.name, got, tc.want)
		}
	}

	pinned := mainnet
	pinned.VaultRotationV2ActivationHeight = 400
	noElection := consensusversion.Version{}
	if KeyCustodyInForce(pinned, 399, noElection) {
		t.Error("below the pinned height with no election: inert")
	}
	if !KeyCustodyInForce(pinned, 400, noElection) {
		t.Error("at the vault-rotation pin the custody rules run with it")
	}
}
