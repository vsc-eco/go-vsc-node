package systemconfig

import (
	"testing"

	"vsc-node/modules/common/params"
)

// VR2-17 regression guard.
//
// RC_HIVE_FREE_AMOUNT used to be a package-level var MUTATED at process start by
// FromNetwork (devnet/mocknet raised it to 1_000_000, mainnet/testnet did not).
// It feeds consensus-critical RC accounting — the WASM gas budget and the
// PullBalance HBD-exclusion — so two nodes on the same network that disagreed on
// it computed different contract results, different block CIDs, and never reached
// quorum: a fleet-wide stall, devnet-proven, and the real cause of the F7
// "mixed-fleet halt" that was long mis-attributed to vault-rotation-v2.
//
// These tests pin the two properties that keep that from recurring:
//  1. Mainnet/testnet MUST stay at the production default. If either is ever left
//     to a zero value, a reindex recomputes every historical Hive-account gas
//     budget and PullBalance exclusion against 0 free RC instead of 10_000 —
//     different results than the chain was actually signed with, i.e. a REPLAY
//     FORK. This is the footgun the council flagged as easy to miss.
//  2. FromNetwork must not mutate the global, so the value is a function of the
//     network config every node shares, not of which binary was compiled.
func TestRcHiveFreeAmount_ProductionNetworksKeepDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  SystemConfig
	}{
		{"mainnet", MainnetConfig()},
		{"testnet", TestnetConfig()},
	} {
		got := tc.cfg.RcHiveFreeAmount()
		if got != params.RC_HIVE_FREE_AMOUNT {
			t.Fatalf("%s RcHiveFreeAmount = %d, want the production default %d "+
				"(a wrong/zero value here forks a reindex)", tc.name, got, params.RC_HIVE_FREE_AMOUNT)
		}
		if got == 0 {
			t.Fatalf("%s RcHiveFreeAmount is 0 — historical replay would recompute "+
				"every Hive-account gas budget against 0 free RC", tc.name)
		}
	}
}

func TestRcHiveFreeAmount_EphemeralNetworksAreRaised(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  SystemConfig
	}{
		{"devnet", DevnetConfig()},
		{"mocknet", MocknetConfig()},
	} {
		if got := tc.cfg.RcHiveFreeAmount(); got != params.RC_HIVE_FREE_AMOUNT_EPHEMERAL {
			t.Fatalf("%s RcHiveFreeAmount = %d, want %d", tc.name, got, params.RC_HIVE_FREE_AMOUNT_EPHEMERAL)
		}
	}
}

// FromNetwork must resolve the value through the network config, never by
// mutating the shared package global (the original defect).
func TestRcHiveFreeAmount_FromNetworkDoesNotMutateGlobal(t *testing.T) {
	before := params.RC_HIVE_FREE_AMOUNT

	// Selecting an ephemeral network previously overwrote the global for the
	// whole process, so a later mainnet/testnet reader saw 1_000_000.
	devnet := FromNetwork("devnet")
	mocknet := FromNetwork("mocknet")

	if params.RC_HIVE_FREE_AMOUNT != before {
		t.Fatalf("FromNetwork mutated params.RC_HIVE_FREE_AMOUNT: %d -> %d "+
			"(this is exactly the VR2-17 divergence)", before, params.RC_HIVE_FREE_AMOUNT)
	}
	if devnet.RcHiveFreeAmount() != params.RC_HIVE_FREE_AMOUNT_EPHEMERAL ||
		mocknet.RcHiveFreeAmount() != params.RC_HIVE_FREE_AMOUNT_EPHEMERAL {
		t.Fatal("ephemeral networks must carry the raised allowance on the config")
	}
	// And the production networks are unaffected by having selected an ephemeral one.
	if FromNetwork("mainnet").RcHiveFreeAmount() != params.RC_HIVE_FREE_AMOUNT {
		t.Fatal("mainnet allowance changed after selecting an ephemeral network")
	}
}
