package systemconfig

import "testing"

// PENDULUM-SLIP-PARITY mainnet activation guard.
//
// PendulumSlipParityHeight switches the swap fee to THORChain's rule
// (max(full slip fee, protocol tier), no total-fee clamp) from a Hive L1
// height. It is a COMPILE-TIME consensus value with NO version gate: every
// mainnet witness must hold the same number, or a fleet split across the
// height charges different swap fees, writes different contract outputs and
// diverges. So the height that was announced to the witnesses is pinned here:
// lowering it strands nodes that have not upgraded, and moving it after the
// chain has passed it rewrites fees that were already charged.
func TestPendulumSlipParityHeight_Mainnet(t *testing.T) {
	// Announced for mainnet 2026-11-26 (L1 head 112,008,791, ~3 days out).
	const mainnetHeight = uint64(112_096_000)

	if got := MainnetConfig().ConsensusParams().PendulumSlipParityHeight; got != mainnetHeight {
		t.Errorf("mainnet PendulumSlipParityHeight = %d, want %d. Changing it moves a "+
			"consensus cutover: every mainnet witness must run the same value, so update the "+
			"announcement and this test together, and never after the chain has passed it.",
			got, mainnetHeight)
	}
}

// The devnet/mocknet harnesses stay on the old fee math by default: their fee
// regressions are written against it, and the devnet proof of the new rule opts
// in through SysConfigOverrides instead.
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
