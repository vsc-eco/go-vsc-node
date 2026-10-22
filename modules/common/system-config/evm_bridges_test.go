package systemconfig

import "testing"

// C-R2: every shipped network's EVM bridges carry an activation height (a node
// refuses to start otherwise).
func TestEveryNetworksEvmBridgesHaveAnActivationHeight(t *testing.T) {
	for _, net := range []string{"mainnet", "testnet", "devnet", "mocknet"} {
		if err := FromNetwork(net).OracleParams().ValidateEvmBridges(); err != nil {
			t.Errorf("%s: %v", net, err)
		}
	}
}
