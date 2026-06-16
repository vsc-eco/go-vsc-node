package pendulumwasm

import (
	"math/big"
	"testing"

	"vsc-node/modules/common/consensusversion"
	pendulum "vsc-node/modules/incentive-pendulum"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	wasm_context "vsc-node/modules/wasm/context"
)

// fixedVersion is a ConsensusVersionAt returning the same triple at every
// height, so floor tests can stand on either side of LPFloorActivation.
func fixedVersion(major, consensus uint64) ConsensusVersionAt {
	return func(uint64) consensusversion.Version {
		return consensusversion.Version{Major: major, Consensus: consensus}
	}
}

// On the under-secured cliff (V ≥ c·E) the raw split routes 100% to nodes; the
// LP floor must cap that at the node ceiling instead.
func TestSplitFractionsBpsFloorCapsCliff(t *testing.T) {
	E := big.NewInt(1_000_000)
	V := big.NewInt(4_000_000) // ≥ c·E = 3·1_000_000 → under-secured
	P := big.NewInt(2_000_000)
	T := big.NewInt(1_000_000)
	sBps := int64(4 * pendulum.BpsScale)

	if fN, fP := splitFractionsBps(T, V, E, P, sBps, pendulum.BpsScale); fN != pendulum.BpsScale || fP != pendulum.BpsScale {
		t.Fatalf("floor off: got (%d,%d), want (10000,10000)", fN, fP)
	}
	maxNode := pendulum.MaxNodeShareBps(1_000) // 10% LP floor → node ceiling 9000
	if fN, fP := splitFractionsBps(T, V, E, P, sBps, maxNode); fN != 9_000 || fP != 9_000 {
		t.Fatalf("floor on: got (%d,%d), want (9000,9000)", fN, fP)
	}
}

// Just below the cliff (s = 2.9) the raw node fraction already exceeds the
// ceiling, and s > RedirectHi sends the protocol leg fully to nodes — both get
// capped by the floor.
func TestSplitFractionsBpsFloorCapsHighS(t *testing.T) {
	E := big.NewInt(1_000_000)
	V := big.NewInt(2_900_000)
	P := big.NewInt(1_450_000)
	T := big.NewInt(1_000_000)
	sBps := int64(29_000)

	fNodeOff, fProtoOff := splitFractionsBps(T, V, E, P, sBps, pendulum.BpsScale)
	if fNodeOff <= 9_000 {
		t.Fatalf("floor off: raw node fraction %d should exceed 9000 in this regime", fNodeOff)
	}
	if fProtoOff != pendulum.BpsScale {
		t.Fatalf("floor off: protocol leg should redirect to nodes (%d), got %d", pendulum.BpsScale, fProtoOff)
	}

	maxNode := pendulum.MaxNodeShareBps(1_000)
	if fNodeOn, fProtoOn := splitFractionsBps(T, V, E, P, sBps, maxNode); fNodeOn != maxNode || fProtoOn != maxNode {
		t.Fatalf("floor on: got (%d,%d), want (%d,%d)", fNodeOn, fProtoOn, maxNode, maxNode)
	}
}

// In the healthy band the node fraction is already under the ceiling, so the
// floor is a strict no-op — the split is byte-identical with and without it.
func TestSplitFractionsBpsFloorNoOpInHealthyBand(t *testing.T) {
	E := big.NewInt(1_000_000)
	V := big.NewInt(1_000_000) // s = 1.0 (equilibrium)
	P := big.NewInt(500_000)
	T := big.NewInt(1_000_000)
	sBps := int64(pendulum.BpsScale)

	offN, offP := splitFractionsBps(T, V, E, P, sBps, pendulum.BpsScale)
	onN, onP := splitFractionsBps(T, V, E, P, sBps, pendulum.MaxNodeShareBps(1_000))
	if onN != offN || onP != offP {
		t.Fatalf("floor must be a no-op below the ceiling: off=(%d,%d) on=(%d,%d)", offN, offP, onN, onP)
	}
	if offN != 5_000 {
		t.Fatalf("equilibrium node fraction = %d, want 5000", offN)
	}
}

// End-to-end: the floor only takes effect once the chain-active consensus
// version reaches LPFloorActivation (0.2.0). Below it — and with no version
// reader at all — the split is the historical 100%-to-nodes cliff.
func TestApplySwapFeesLPFloorGatedByConsensus(t *testing.T) {
	wl := []string{"contract:pool-1"}
	cfg := Config{
		Stabilizer:      pendulum.DefaultStabilizerParamsBps(),
		NetworkShareNum: 1,
		NetworkShareDen: 4,
		MinFractionBps:  1_000, // 10% LP floor
	}
	geo := pendulumoracle.GeometryOutputs{
		OK: true, V: 4_000_000, P: 2_000_000, E: 1_000_000, T: 1_000_000,
		SBps: 4 * pendulum.BpsScale, // under-secured cliff
	}
	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn: "hbd", AssetOut: "hive",
		X: 10_000_000, XReserve: 1_000_000_000, YReserve: 1_000_000_000,
	}

	apply := func(cv ConsensusVersionAt) wasm_context.PendulumSwapFeeResult {
		a := New(&stubGeometry{out: geo}, func() []string { return wl }, cv, cfg)
		res := a.ApplySwapFees("contract:pool-1", "tx", 100, args, (&recordingAccrual{}).fn)
		if res.IsErr() {
			t.Fatalf("apply failed: %v", res)
		}
		return res.Unwrap()
	}

	active := apply(fixedVersion(0, 2)) // ≥ 0.2.0 → floor enforced
	pre := apply(fixedVersion(0, 1))    // 0.1.0 (below activation) → floor inert
	nilCV := apply(nil)                 // no reader → floor inert

	if active.LpShareOutput <= 0 {
		t.Fatalf("floor active: LPs must keep a positive share on the cliff, got %d", active.LpShareOutput)
	}
	if pre.LpShareOutput != 0 {
		t.Fatalf("pre-activation: cliff routes 100%% to nodes, want LP share 0, got %d", pre.LpShareOutput)
	}
	if nilCV.LpShareOutput != 0 {
		t.Fatalf("nil consensus reader: floor must stay inert, got LP share %d", nilCV.LpShareOutput)
	}
	if active.NodeShareOutput >= pre.NodeShareOutput {
		t.Fatalf("floor active node share %d must be below pre-activation %d", active.NodeShareOutput, pre.NodeShareOutput)
	}
	// The floor only redistributes the pendulum pot between LP and node — it
	// must not resize the pot or touch the network cut.
	if active.LpShareOutput+active.NodeShareOutput != pre.LpShareOutput+pre.NodeShareOutput {
		t.Fatalf("pot resized: active %d != pre %d",
			active.LpShareOutput+active.NodeShareOutput, pre.LpShareOutput+pre.NodeShareOutput)
	}
	if active.NetworkCreditOutput != pre.NetworkCreditOutput {
		t.Fatalf("network cut changed: active %d != pre %d", active.NetworkCreditOutput, pre.NetworkCreditOutput)
	}
}
