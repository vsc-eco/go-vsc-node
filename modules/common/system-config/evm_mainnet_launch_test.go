package systemconfig

import (
	"testing"

	"vsc-node/modules/common/params"
)

const (
	mainnetEvmBridge   = "vsc1BqoXD32CWWx9UBfh18TnN7Es85RBut2Uhr"
	mainnetZkVerifier  = "vsc1BhQyZEM73tdE9oafqkwzjD6uSoPLriN68z"
	mainnetEthHbdPool  = "vsc1BfKKwFx6uHWMyiaXyjcT8cvMRRfXTvXd6z"
	mainnetHbdUsdcPool = "vsc1Bo8BqNty9d4s1BuadHSJ1Lc3G4fAyzwHpr"
	mainnetBtcMapping  = "vsc1BdrQ6EtbQ64rq2PkPd21x4MaLnVRcJj85d"
)

// The EVM launch height is ahead of the head it was pinned from, so the
// launch rolls out before it binds (a past height fails open).
func TestMainnetEvmLaunch_HeightIsAhead(t *testing.T) {
	const pinnedFromHead = 111_809_396
	if params.EVM_MAINNET_HEIGHT <= pinnedFromHead+2_400 {
		t.Fatalf("EVM_MAINNET_HEIGHT %d must be at least 2,400 blocks past %d", params.EVM_MAINNET_HEIGHT, pinnedFromHead)
	}
}

// The bridge is a consensus input from the launch height only, and the oracle
// never relays Ethereum headers (they come from the ZK verifier).
func TestMainnetEvmLaunch_BridgeFromItsHeight(t *testing.T) {
	op := MainnetConfig().OracleParams()
	h := params.EVM_MAINNET_HEIGHT
	if op.EvmBridgeContracts["ETH"] != mainnetEvmBridge || op.EvmBridgeFromHeight["ETH"] != h {
		t.Fatalf("mainnet bridge map = %v from %v", op.EvmBridgeContracts, op.EvmBridgeFromHeight)
	}
	if ids := op.EvmBridgeContractIds(h - 1); len(ids) != 0 {
		t.Fatalf("below the height no bridge is a consensus input, got %v", ids)
	}
	if ids := op.EvmBridgeContractIds(h); len(ids) != 1 || ids[0] != mainnetEvmBridge {
		t.Fatalf("from the height the bridge is a consensus input, got %v", ids)
	}
	if !op.HasZKVerifier("ETH") || op.ZKVerifierChains["ETH"] != mainnetZkVerifier {
		t.Fatalf("ETH headers come from the ZK verifier, got %v", op.ZKVerifierChains)
	}
	if op.ContractId("ETH") != "" {
		t.Fatal("ETH must have no BLS-relay contract")
	}
}

// The bridge may create its TSS vault key from the launch height; the BTC
// mapping contract keeps its place at every height.
func TestMainnetEvmLaunch_TssAllowlistFromItsHeight(t *testing.T) {
	c := MainnetConfig()
	h := params.EVM_MAINNET_HEIGHT
	before, at := c.TssKeyContractAllowlistAt(h-1), c.TssKeyContractAllowlistAt(h)
	if has(before, mainnetEvmBridge) || !has(before, mainnetBtcMapping) {
		t.Fatalf("below the height: %v", before)
	}
	if !has(at, mainnetEvmBridge) || !has(at, mainnetBtcMapping) || len(at) != 2 {
		t.Fatalf("from the height the allowlist is the BTC mapping contract and the EVM bridge, got %v", at)
	}
}

// ETH:HBD and HBD:USDC swap and count as collateral from the launch height,
// not before; the existing pools are unchanged.
func TestMainnetEvmLaunch_PoolsFromTheirHeight(t *testing.T) {
	c := MainnetConfig().(*config)
	h := params.EVM_MAINNET_HEIGHT
	for _, id := range []string{mainnetEthHbdPool, mainnetHbdUsdcPool} {
		if has(c.PendulumPoolWhitelistAt(h-1), id) || has(c.PendulumCollateralPoolsAt(h-1), id) {
			t.Fatalf("%s must not swap or count before the height", id)
		}
		if !has(c.PendulumPoolWhitelistAt(h), id) || !has(c.PendulumCollateralPoolsAt(h), id) {
			t.Fatalf("%s must swap and count as collateral from the height", id)
		}
	}
	if got, want := c.PendulumCollateralPoolsAt(h), mainnetCollateralAt(h); !sameStringSet(got, want) {
		t.Fatalf("collateral at the height = %v, want %v", got, want)
	}
}
