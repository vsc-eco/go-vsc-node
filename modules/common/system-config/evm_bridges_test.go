package systemconfig

import (
	"os"
	"testing"
)

// C-R2: every shipped network's EVM bridges carry an activation height (a node
// refuses to start otherwise).
func TestEveryNetworksEvmBridgesHaveAnActivationHeight(t *testing.T) {
	for _, net := range []string{"mainnet", "testnet", "devnet", "mocknet"} {
		if err := FromNetwork(net).OracleParams().ValidateEvmBridges(); err != nil {
			t.Errorf("%s: %v", net, err)
		}
	}
}

// A21: on mainnet and testnet the EVM bridge map is the build's: an operator's
// override of it is ignored (it is a consensus input from 0.13.0, and a
// per-operator map gave nodes different answers). Other oracle overrides
// still apply, and devnet keeps the override (the test waves wire their own
// bridge with it).
func TestA21_EvmBridgeMapIsTheBuildsOnPublicNetworks(t *testing.T) {
	path := t.TempDir() + "/sysconfig.json"
	if err := os.WriteFile(path, []byte(`{"oracleParams":{"evmBridgeContracts":{"ETH":"vsc1Operators"},"evmBridgeFromHeight":{"ETH":5},"btcL1BaseURL":"https://example.invalid/api"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, net := range []string{"mainnet", "testnet"} {
		built := FromNetwork(net).OracleParams()
		c := FromNetwork(net)
		if err := c.LoadOverrides(path); err != nil {
			t.Fatal(err)
		}
		op := c.OracleParams()
		if !sameMap(op.EvmBridgeContracts, built.EvmBridgeContracts) || !sameMap(op.EvmBridgeFromHeight, built.EvmBridgeFromHeight) {
			t.Errorf("%s: the operator's bridge map took effect: %v %v, the build's is %v %v", net,
				op.EvmBridgeContracts, op.EvmBridgeFromHeight, built.EvmBridgeContracts, built.EvmBridgeFromHeight)
		}
		for _, id := range op.EvmBridgeContractIds(1_000_000) {
			if id == "vsc1Operators" {
				t.Errorf("%s: the operator's bridge is a consensus input", net)
			}
		}
		if op.BtcL1BaseURL != "https://example.invalid/api" {
			t.Errorf("%s: another oracle override was dropped too", net)
		}
	}
	c := FromNetwork("devnet")
	if err := c.LoadOverrides(path); err != nil {
		t.Fatal(err)
	}
	if ids := c.OracleParams().EvmBridgeContractIds(5); len(ids) != 1 || ids[0] != "vsc1Operators" {
		t.Fatalf("devnet: %v, want the override", ids)
	}
}
