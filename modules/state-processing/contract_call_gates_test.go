package state_engine

import (
	"testing"

	"vsc-node/modules/common/consensusversion"
	systemconfig "vsc-node/modules/common/system-config"
)

// ContractCallGates resolves the TSS key allowlist from the network's config at
// the version it starts (0.14.0, TssKeyAllowlistActive), and copes with a state engine that has no
// system config.
func TestContractCallGates_AllowlistFromTheConfig(t *testing.T) {
	v := func(c uint64) consensusversion.Version { return consensusversion.Version{Consensus: c} }
	main := systemconfig.MainnetConfig()
	if got := tssKeyAllowlistFor(main, 111_000_000, v(13)); got != nil {
		t.Fatalf("0.13.0: allowlist %v, want nil (not in force yet)", got)
	}
	got := tssKeyAllowlistFor(main, 111_000_000, v(14))
	if len(got) == 0 {
		t.Fatal("0.14.0 on mainnet: the allowlist is empty")
	}
	if n := len(ContractCallGates(main, 111_000_000, v(14))); n != 9 {
		t.Fatalf("%d gate options, want 9: a new gate needs a line in TestSimulateRunsWithTheChainsGates (modules/gql/gqlgen)", n)
	}
	if n := len(ContractCallGates(nil, 1, v(14))); n != 9 {
		t.Fatalf("nil system config: %d gate options, want 9", n)
	}
}
