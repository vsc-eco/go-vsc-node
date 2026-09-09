package state_engine

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	"vsc-node/modules/common/system-config"
)

// VR2-02: the vault-rotation-v2 cutover coordinates on the ATTESTED consensus
// version floor, with the activation height retained only as an ephemeral-network
// override.
//
// Why this matters more than it looks. A bare height pin requires every witness to
// be running a binary that carries the pinned height BEFORE the chain reaches it.
// Nodes that upgrade late compute different results across the gap, and because
// block signing independently re-derives the block and byte-compares the CID, the
// two halves of a mixed fleet simply cannot co-sign: the network stalls. The
// version floor removes that failure mode by construction — it cannot rise to
// 0.8.0 until a stake-supermajority attests it is RUNNING code that implements the
// batch, so a laggard fails to drag the floor up instead of silently diverging.
//
// The height pin is kept because a floor-only gate is INERT at genesis: a
// fresh-genesis network has no stored election, so the chain-active version
// resolves to 0.0.0 no matter what is configured. Ephemeral networks pin the
// height and get v2 from block 1; mainnet leaves it 0 and uses the attested floor.
func TestVaultRotationV2InForce(t *testing.T) {
	mainnet := systemconfig.MainnetConfig().ConsensusParams()

	// Sanity: the shipped mainnet config must not pin a height. If this ever fails,
	// the footgun this change removes has been reintroduced by configuration.
	if mainnet.VaultRotationV2ActivationHeight != 0 {
		t.Fatalf("mainnet must not pin a vault-v2 activation height; got %d",
			mainnet.VaultRotationV2ActivationHeight)
	}

	const bh = uint64(1_000_000)

	for _, tc := range []struct {
		name   string
		active consensusversion.Version
		want   bool
	}{
		{"no election stored yet", consensusversion.Version{}, false},
		{"mainnet's floor today (0.3.0)", consensusversion.Version{Major: 0, Consensus: 3}, false},
		{"POA shipped but not vault-v2 (0.7.0)", consensusversion.Version{Major: 0, Consensus: 7}, false},
		{"the vault-v2 line (0.8.0)", consensusversion.Version{Major: 0, Consensus: 8}, true},
		{"a later line still carries it", consensusversion.Version{Major: 0, Consensus: 9}, true},
	} {
		if got := VaultRotationV2InForce(mainnet, bh, tc.active); got != tc.want {
			t.Errorf("%s: VaultRotationV2InForce(active=%s) = %v, want %v",
				tc.name, tc.active.Format(), got, tc.want)
		}
	}
}

// The height pin still wins where it is set, which is what keeps a fresh-genesis
// devnet working: it has no election, so the floor half can never answer true.
func TestVaultRotationV2HeightPinOverridesAnAbsentFloor(t *testing.T) {
	cp := systemconfig.MainnetConfig().ConsensusParams()
	cp.VaultRotationV2ActivationHeight = 400

	noElection := consensusversion.Version{} // exactly what a fresh genesis resolves to

	if VaultRotationV2InForce(cp, 399, noElection) {
		t.Error("below the pinned height with no election, v2 must be inert")
	}
	if !VaultRotationV2InForce(cp, 400, noElection) {
		t.Error("at the pinned height, v2 must be active even with no election stored — " +
			"this is the case that makes fresh-genesis ephemeral networks work")
	}
	if !VaultRotationV2InForce(cp, 100_000, noElection) {
		t.Error("above the pinned height, v2 must stay active")
	}
}

// Every shipped network must be inert by configuration, so the batch cannot
// activate anywhere until a floor rise deliberately turns it on.
func TestVaultRotationV2IsUnpinnedOnEveryShippedNetwork(t *testing.T) {
	for _, network := range []string{"mainnet", "testnet", "devnet", "mocknet"} {
		cp := systemconfig.FromNetwork(network).ConsensusParams()
		if cp.VaultRotationV2ActivationHeight != 0 {
			t.Errorf("%s pins a vault-v2 activation height (%d); the attested version floor "+
				"is the intended path and a pin reintroduces the rolling-upgrade footgun",
				network, cp.VaultRotationV2ActivationHeight)
		}
	}
}
