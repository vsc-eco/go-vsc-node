package systemconfig

import "testing"

// PENDULUM-SLIP-PARITY activation guard
// (00-README-ORDER-AND-SLIP-FEE-ACTIVATION.md step 1, go-vsc-node #309).
//
// PendulumSlipParityHeight switches the swap fee to THORChain's rule
// (max(full slip fee, protocol tier), no total-fee clamp) from a Hive L1
// height. It is a COMPILE-TIME consensus value with NO version gate: every
// node on a network must hold the same number, or a fleet split across the
// height charges different swap fees, writes different contract outputs and
// diverges. That makes it the kind of constant a well-meaning edit quietly
// breaks, so the two properties that matter are pinned here:
//
//  1. Testnet activates at the height that was announced to its witnesses.
//     Lowering it strands nodes that have not upgraded; raising it after the
//     chain has passed it rewrites fees that were already charged, so a reindex
//     disagrees with the signed history.
//  2. Mainnet stays INERT (0) until its fleet is coordinated. Pinning a mainnet
//     height is a deliberate, separate act — never a side effect of a testnet
//     change, and never a copy-paste of the testnet value, whose L1 heights are
//     a different chain's and millions of blocks apart.
func TestPendulumSlipParityHeight_PerNetwork(t *testing.T) {
	// Announced for testnet 2026-11-22 (L1 head ~8,472,460, ~7 days out).
	const testnetHeight = uint64(8_674_000)

	if got := TestnetConfig().ConsensusParams().PendulumSlipParityHeight; got != testnetHeight {
		t.Errorf("testnet PendulumSlipParityHeight = %d, want %d. Changing it moves a "+
			"consensus cutover: every testnet witness must run the same value, so update the "+
			"announcement and this test together, and never after the chain has passed it.",
			got, testnetHeight)
	}

	if got := MainnetConfig().ConsensusParams().PendulumSlipParityHeight; got != 0 {
		t.Errorf("mainnet PendulumSlipParityHeight = %d, want 0 (inert). A mainnet height is "+
			"pinned only once the witness fleet is coordinated onto the release that carries it "+
			"and the params.go DEPLOY ORDER preconditions hold on mainnet; a mixed fleet across "+
			"the height diverges.", got)
	}
}

// The devnet/mocknet harnesses must stay on the old fee math by default: their
// fee regressions are written against it, and the devnet proof of the new rule
// pins the height to 1 through SysConfigOverrides instead.
func TestPendulumSlipParityHeight_TestHarnessesStayInert(t *testing.T) {
	for name, cfg := range map[string]SystemConfig{
		"devnet":  DevnetConfig(),
		"mocknet": MocknetConfig(),
	} {
		if got := cfg.ConsensusParams().PendulumSlipParityHeight; got != 0 {
			t.Errorf("%s PendulumSlipParityHeight = %d, want 0: the harness opts in per test "+
				"(SysConfigOverrides), so the default stays the pre-parity fee math", name, got)
		}
	}
}
