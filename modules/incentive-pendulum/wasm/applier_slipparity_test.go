package pendulumwasm

import (
	"math/big"
	"testing"

	"vsc-node/lib/intmath"
	pendulum "vsc-node/modules/incentive-pendulum"
	wasm_context "vsc-node/modules/wasm/context"
)

// SlipParityHeight: THORChain's swap fee (x/thorchain/swap_current.go
// GetSwapCalc): the slip fee x²·Y/(x+X)² in full, floored at a minimum (here
// the protocol tier, 8 bps × stabilizer m), no total-fee clamp.

func parityApplier(cfg Config) *Applier {
	return New(&stubGeometry{out: incidentGeometry()}, func(uint64) []string { return []string{"contract:pool-1"} }, nil, cfg)
}

// baseFees recomputes grossOut, baseCLP and baseProtocol with the applier's
// integer ops for any constant-product swap.
func baseFees(a wasm_context.PendulumSwapFeeArgs) (grossOut, baseCLP, baseProtocol *big.Int) {
	x := big.NewInt(a.X)
	xPlusX := big.NewInt(a.X + a.XReserve)
	y := big.NewInt(a.YReserve)
	grossOut = intmath.MulDivFloor(x, y, xPlusX)
	baseCLP = intmath.MulDivFloor(new(big.Int).Mul(x, x), y, new(big.Int).Mul(xPlusX, xPlusX))
	baseProtocol = intmath.MulDivFloor(grossOut, big.NewInt(8), big.NewInt(pendulum.BpsScale))
	return
}

func swapFee(t *testing.T, a *Applier, bh uint64, args wasm_context.PendulumSwapFeeArgs) (fee *big.Int, out wasm_context.PendulumSwapFeeResult) {
	t.Helper()
	res := a.ApplySwapFees("contract:pool-1", "tx", bh, args, (&recordingAccrual{}).fn)
	if res.IsErr() {
		t.Fatalf("swap at %d failed: %v", bh, res)
	}
	out = res.Unwrap()
	gross, _, _ := baseFees(args)
	return new(big.Int).Sub(gross, big.NewInt(out.UserOutput)), out
}

// expectedParityFee is max(baseCLP, protocol tier), the protocol tier being
// baseProtocol × m floored at baseProtocol (as the applier charges it).
func expectedParityFee(args wasm_context.PendulumSwapFeeArgs, mBps int64) *big.Int {
	_, baseCLP, baseProtocol := baseFees(args)
	protocol := pendulum.ApplyMultiplierBps(baseProtocol, mBps)
	if protocol.Cmp(baseProtocol) < 0 {
		protocol = baseProtocol
	}
	if baseCLP.Cmp(protocol) > 0 {
		return baseCLP
	}
	return protocol
}

func smallArgs() wasm_context.PendulumSwapFeeArgs {
	// 0.04% of the pool: the slip fee is below the protocol tier.
	return wasm_context.PendulumSwapFeeArgs{AssetIn: "hbd", AssetOut: "hive", X: 1_000, XReserve: 2_474_000, YReserve: 47_755_000}
}

func TestSlipParity_InertByDefault(t *testing.T) {
	if DefaultConfig().SlipParityHeight != 0 {
		t.Fatal("the THORChain fee rule must be off unless a height is pinned")
	}
	on := DefaultConfig()
	on.SlipParityHeight = 0
	for _, args := range []wasm_context.PendulumSwapFeeArgs{incidentArgs(), smallArgs()} {
		a, _ := swapFee(t, parityApplier(DefaultConfig()), 100, args)
		b, _ := swapFee(t, parityApplier(on), 100, args)
		if a.Cmp(b) != 0 {
			t.Fatalf("height 0 changed the fee: %s vs %s", a, b)
		}
	}
}

func TestSlipParity_LargeSwapPaysTheFullSlipFeeUncapped(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SlipParityHeight = 50
	fee, out := swapFee(t, parityApplier(cfg), 100, incidentArgs())

	want := expectedParityFee(incidentArgs(), int64(out.MultiplierBps))
	if fee.Cmp(want) != 0 {
		t.Fatalf("fee %s, want max(slip fee, protocol tier) = %s", fee, want)
	}
	gross, baseCLP, _ := baseFees(incidentArgs())
	if fee.Cmp(baseCLP) != 0 {
		t.Fatalf("a 3%% swap must pay the full slip fee %s, got %s", baseCLP, fee)
	}
	// THORChain caps no fee: above the old 1% clamp.
	if fee.Cmp(intmath.MulDivFloor(gross, big.NewInt(100), big.NewInt(pendulum.BpsScale))) <= 0 {
		t.Fatalf("fee %s is still under the 1%% clamp", fee)
	}
	// Below the pre-June overcharge (full slip × m plus protocol × m).
	if fee.Cmp(intmath.MulDivFloor(gross, big.NewInt(500), big.NewInt(pendulum.BpsScale))) >= 0 {
		t.Fatalf("fee %s reaches the pre-June overcharge regime", fee)
	}
	if split := big.NewInt(out.LpShareOutput + out.NodeShareOutput + out.NetworkCreditOutput); split.Cmp(fee) != 0 {
		t.Fatalf("lp+node+network %s does not conserve to the fee %s", split, fee)
	}
}

func TestSlipParity_SmallSwapPaysTheProtocolTierOnly(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SlipParityHeight = 50
	fee, out := swapFee(t, parityApplier(cfg), 100, smallArgs())
	want := expectedParityFee(smallArgs(), int64(out.MultiplierBps))
	if fee.Cmp(want) != 0 {
		t.Fatalf("fee %s, want %s", fee, want)
	}
	today, _ := swapFee(t, parityApplier(DefaultConfig()), 100, smallArgs())
	if fee.Cmp(today) > 0 {
		t.Fatalf("a small swap must not pay more than today: %s > %s", fee, today)
	}
	if split := big.NewInt(out.LpShareOutput + out.NodeShareOutput + out.NetworkCreditOutput); split.Cmp(fee) != 0 {
		t.Fatalf("lp+node+network %s does not conserve to the fee %s", split, fee)
	}
}

func TestSlipParity_HeightGate(t *testing.T) {
	const H = 120_000_000
	cfg := DefaultConfig()
	cfg.SlipParityHeight = H
	today, _ := swapFee(t, parityApplier(DefaultConfig()), H-1, incidentArgs())
	before, _ := swapFee(t, parityApplier(cfg), H-1, incidentArgs())
	at, out := swapFee(t, parityApplier(cfg), H, incidentArgs())
	if before.Cmp(today) != 0 {
		t.Fatalf("below the height the fee must be today's: %s vs %s", before, today)
	}
	if want := expectedParityFee(incidentArgs(), int64(out.MultiplierBps)); at.Cmp(want) != 0 {
		t.Fatalf("at the height the fee must be the THORChain rule: %s, want %s", at, want)
	}
}

// Reserve rule unchanged (TestReserveConservationHBDIn): the user's input
// enters X and the node share leaves X as HBD; on Y only the user's output
// leaves, the fees stay in the reserves.
func TestSlipParity_ReservesConserve(t *testing.T) {
	cfg := DefaultConfig()
	cfg.SlipParityHeight = 1
	args := incidentArgs()
	_, out := swapFee(t, parityApplier(cfg), 100, args)
	if want := args.XReserve + args.X - out.NodeBucketCreditedHBD; out.NewXReserve != want {
		t.Fatalf("X side %d, want %d", out.NewXReserve, want)
	}
	if want := args.YReserve - out.UserOutput; out.NewYReserve != want {
		t.Fatalf("Y side %d, want %d", out.NewYReserve, want)
	}
}
