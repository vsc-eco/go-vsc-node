package pendulumwasm

import (
	"math/big"
	"testing"

	"vsc-node/lib/intmath"
	pendulum "vsc-node/modules/incentive-pendulum"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	wasm_context "vsc-node/modules/wasm/context"
)

// stubGeometry is a deterministic GeometryReader for unit tests.
type stubGeometry struct {
	out pendulumoracle.GeometryOutputs
}

func (s *stubGeometry) GeometryAt(_ uint64) (pendulumoracle.GeometryOutputs, bool) {
	return s.out, s.out.OK
}

// recordingAccrual captures every AccrueNodeBucketFn invocation so tests can
// assert on the bucket movement without standing up a LedgerSession.
type recordingAccrual struct {
	calls []int64
}

func (r *recordingAccrual) fn(amountHBD int64) error {
	r.calls = append(r.calls, amountHBD)
	return nil
}

func defaultArgs(assetIn, assetOut string) wasm_context.PendulumSwapFeeArgs {
	return wasm_context.PendulumSwapFeeArgs{
		AssetIn:  assetIn,
		AssetOut: assetOut,
		X:        1_000,     // 0.001 of input asset (small swap)
		XReserve: 1_000_000, // 1.0 input asset reserve
		YReserve: 1_000_000, // 1.0 output asset reserve
	}
}

func balancedGeometry() pendulumoracle.GeometryOutputs {
	// V=1_000_000, E=1_000_000, P=500_000, T=1_000_000 → s = V/E = 1.0 (the new
	// equilibrium). Not a cliff (cliff is V ≥ 3E).
	return pendulumoracle.GeometryOutputs{
		OK:   true,
		V:    1_000_000,
		P:    500_000,
		E:    1_000_000,
		T:    1_000_000,
		SBps: pendulum.BpsScale, // s = 1.0 → 10000 bps
	}
}

func newApplier(t *testing.T, geo pendulumoracle.GeometryOutputs, whitelist []string) (*Applier, *recordingAccrual) {
	t.Helper()
	a := New(
		&stubGeometry{out: geo},
		func() []string { return whitelist },
		DefaultConfig(),
	)
	return a, &recordingAccrual{}
}

// TestRejectsNonWhitelistedContract is the first guard the SDK method
// applies — calls from contracts not in the whitelist produce a clean error
// without invoking the accrual callback or snapshot DB.
func TestRejectsNonWhitelistedContract(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:other"})
	res := a.ApplySwapFees("contract:not-whitelisted", "tx-1", 100, defaultArgs("hbd", "hive"), acc.fn)
	if !res.IsErr() {
		t.Fatal("expected error for non-whitelisted contract")
	}
	if len(acc.calls) != 0 {
		t.Fatalf("expected no accrual calls, got %d", len(acc.calls))
	}
}

// TestRejectsMissingGeometry guards against pre-warmup swap calls — until
// the geometry computer has bond + price data, OK == false should refuse the
// swap rather than silently misprice.
func TestRejectsMissingGeometry(t *testing.T) {
	geo := balancedGeometry()
	geo.OK = false
	a, acc := newApplier(t, geo, []string{"contract:pool-1"})
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hbd", "hive"), acc.fn)
	if !res.IsErr() {
		t.Fatal("expected error for missing snapshot")
	}
}

// TestRejectsNonHBDPair confirms the testnet-only HBD-paired requirement.
func TestRejectsNonHBDPair(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hive", "btc"), acc.fn)
	if !res.IsErr() {
		t.Fatal("expected error for non-HBD-paired swap")
	}
}

// TestSwapHBDInAccruesNodeBucket exercises a HBD→ASSET1 swap end to end:
// the output asset is non-HBD, so the entire node-share (CLP+protocol)
// goes through one secondary CPMM hop (ASSET1 → HBD) and lands in
// pendulum:nodes:HBD.
func TestSwapHBDInAccruesNodeBucket(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd",
		AssetOut: "hive",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}

	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}

	out := res.Unwrap()
	if out.NodeBucketCreditedHBD <= 0 {
		t.Fatalf("expected positive node bucket credit, got %d", out.NodeBucketCreditedHBD)
	}
	if len(acc.calls) != 1 {
		t.Fatalf("expected exactly one accrual call, got %d", len(acc.calls))
	}
	if acc.calls[0] != out.NodeBucketCreditedHBD {
		t.Fatalf("accrual amount %d != reported credit %d", acc.calls[0], out.NodeBucketCreditedHBD)
	}
	if out.UserOutput <= 0 {
		t.Fatalf("expected positive user output, got %d", out.UserOutput)
	}
	if out.NewXReserve <= 0 || out.NewYReserve <= 0 {
		t.Fatalf("expected positive new reserves, got X=%d Y=%d", out.NewXReserve, out.NewYReserve)
	}
}

// TestSwapASSET1InAccruesNodeBucket runs the mirror direction: ASSET1→HBD.
// Output asset is HBD, so the node share passes through directly with no
// secondary swap; nodeBucketHBD == nodeShareOutput exactly.
func TestSwapASSET1InAccruesNodeBucket(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}

	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}
	out := res.Unwrap()
	if out.NodeBucketCreditedHBD <= 0 {
		t.Fatalf("expected positive node bucket credit, got %d", out.NodeBucketCreditedHBD)
	}
	if len(acc.calls) != 1 {
		t.Fatalf("expected one accrual call, got %d", len(acc.calls))
	}
}

// TestUnderSecuredCliffRoutesAllToNodes locks in the V≥3E cliff: when the vault
// outweighs the bond past the cliff, the entire pendulum pot routes to nodes.
func TestUnderSecuredCliffRoutesAllToNodes(t *testing.T) {
	geo := balancedGeometry()
	geo.V = geo.E*3 + 1 // V > 3E → cliff
	geo.SBps = pendulum.BpsScale * 31 / 10
	a, acc := newApplier(t, geo, []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd",
		AssetOut: "hive",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}

	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}
	if len(acc.calls) != 1 {
		t.Fatalf("expected one accrual call, got %d", len(acc.calls))
	}
	// Cliff: f_node = 1, so all of the pendulum pot goes to nodes. The accrued
	// amount should be greater than zero and roughly proportional to 75% of
	// total fees (the network 25% stays in pool reserves).
	if acc.calls[0] <= 0 {
		t.Fatalf("expected positive node accrual under cliff, got %d", acc.calls[0])
	}
}

// TestNetworkCreditIsSingleOutputAssetValue pins the unified-output-side
// invariant: the network credit is one int64 in the output asset
// (covering 25% of total CLP + 25% of total protocol fee — both of which
// live on the output side under the post-rewrite model).
func TestNetworkCreditIsSingleOutputAssetValue(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}
	out := res.Unwrap()
	if out.NetworkCreditOutput <= 0 {
		t.Fatalf("expected positive single-value network credit, got %d", out.NetworkCreditOutput)
	}
}

// TestReserveConservation pins the no-loss invariant for the HBD-output
// case: every base unit either reaches the user, accrues to the node bucket,
// or stays in the pool. The X side (HBD-paired pool, X is non-HBD here)
// gains exactly the user's input.
func TestReserveConservation(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	xIn := int64(10_000)
	xRes := int64(1_000_000)
	yRes := int64(1_000_000)
	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        xIn,
		XReserve: xRes,
		YReserve: yRes,
	}
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}
	out := res.Unwrap()

	// X side: pure addition of the user's input (no conversion when output is HBD).
	if out.NewXReserve != xRes+xIn {
		t.Fatalf("X side: got %d want %d", out.NewXReserve, xRes+xIn)
	}
	// Y side: starting Y minus user's output minus node-share withdrawal.
	wantY := yRes - out.UserOutput - out.NodeBucketCreditedHBD
	if out.NewYReserve != wantY {
		t.Fatalf("Y side: got %d want %d (Y=%d - user=%d - node=%d)", out.NewYReserve, wantY, yRes, out.UserOutput, out.NodeBucketCreditedHBD)
	}
	// Accrual call matches reported credit.
	if len(acc.calls) != 1 || acc.calls[0] != out.NodeBucketCreditedHBD {
		t.Fatalf("accrual != reported: %+v vs %d", acc.calls, out.NodeBucketCreditedHBD)
	}
}

// TestReserveConservationHBDIn pins the no-loss invariant for the HBD-input
// (non-HBD-output) case: the secondary CPMM hop withdraws the node share as
// HBD from the X side, so newX accounts for both the user's input and the
// node bucket withdrawal. The Y side (non-HBD) only loses the user's output —
// all fees stay in the pool, no double-counting.
//
// This is the mirror of TestReserveConservation and would have caught the
// pre-fix bug where `newY.Add(newY, nodeShareOutput)` ran after Y had already
// been reduced by userOutput-with-fees-retained, inflating Y by nodeShareOutput.
func TestReserveConservationHBDIn(t *testing.T) {
	a, acc := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	xIn := int64(10_000)
	xRes := int64(1_000_000)
	yRes := int64(1_000_000)
	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd",
		AssetOut: "hive",
		X:        xIn,
		XReserve: xRes,
		YReserve: yRes,
	}
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args, acc.fn)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}
	out := res.Unwrap()

	// X side (HBD-paired): user's input enters X, node share leaves X as HBD.
	wantX := xRes + xIn - out.NodeBucketCreditedHBD
	if out.NewXReserve != wantX {
		t.Fatalf("X side: got %d want %d (X=%d + in=%d - node=%d)", out.NewXReserve, wantX, xRes, xIn, out.NodeBucketCreditedHBD)
	}
	// Y side (non-HBD): only the user's output leaves; fees stay implicit.
	wantY := yRes - out.UserOutput
	if out.NewYReserve != wantY {
		t.Fatalf("Y side: got %d want %d (Y=%d - user=%d)", out.NewYReserve, wantY, yRes, out.UserOutput)
	}
	// Accrual call matches reported credit.
	if len(acc.calls) != 1 || acc.calls[0] != out.NodeBucketCreditedHBD {
		t.Fatalf("accrual != reported: %+v vs %d", acc.calls, out.NodeBucketCreditedHBD)
	}
}

// TestStabilizerMultiplierRidesProtocolLegOnly pins the post-incident policy
// (it reverses review-and-plan.md issue #7): the stabilizer multiplier m rides
// the flat protocol leg ONLY. The CLP (slip) leg is scaled by cfg.CLPScaleBps and
// is independent of m, so a high m can no longer double the price-impact-sized
// CLP fee. Load-bearing assertion: the extra fee when m climbs 1.0→1.4 is just
// baseProtocol·(m−1) (a couple base units), NOT the baseCLP·(m−1) blowup (~39
// units here) the old "m on full fee" code charged.
func TestStabilizerMultiplierRidesProtocolLegOnly(t *testing.T) {
	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd", // HBD-in raises s; with s ≥ s_eq, this exacerbates → push=1.0.
		AssetOut: "hive",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}

	// Baseline: s = 1.0 (s_eq) → m = 1.0.
	aBase, accBase := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})
	resBase := aBase.ApplySwapFees("contract:pool-1", "tx-1", 100, args, accBase.fn)
	if resBase.IsErr() {
		t.Fatalf("baseline swap failed: %v", resBase)
	}
	outBase := resBase.Unwrap()
	if outBase.MultiplierBps != pendulum.BpsScale {
		t.Fatalf("expected m == 1.0 at s=1.0, got %d bps", outBase.MultiplierBps)
	}

	// Off-equilibrium: s = 1.2 with V=1_200_000 (P=600_000=V/2 keeps geometry
	// consistent). At r = x/X = 1% and r0 = 1% → r/r0 = 1.0 → inner = 2.0.
	// tail = K · |s−s_eq| · inner · push = 1 · 0.2 · 2 · 1 = 0.4 → m = 1.4.
	geoHigh := balancedGeometry()
	geoHigh.V = 1_200_000
	geoHigh.P = 600_000
	geoHigh.SBps = pendulum.BpsScale * 120 / 100
	aHigh, accHigh := newApplier(t, geoHigh, []string{"contract:pool-1"})
	resHigh := aHigh.ApplySwapFees("contract:pool-1", "tx-2", 100, args, accHigh.fn)
	if resHigh.IsErr() {
		t.Fatalf("off-equilibrium swap failed: %v", resHigh)
	}
	outHigh := resHigh.Unwrap()
	if outHigh.MultiplierBps != pendulum.BpsScale*14/10 {
		t.Fatalf("expected m == 1.4 at s=1.2, r=1%%, got %d bps", outHigh.MultiplierBps)
	}

	// Reserves identical → grossOut, baseCLP, baseProtocol identical. Any drop in
	// userOutput is the extra fee m adds. With m on the protocol leg only:
	//   grossOut       = floor(10000·1_000_000 / 1_010_000) = 9900
	//   baseProtocol   = floor(9900 · 8 / 10000) = 7
	//   chargedProtocol(1.0) = 7 ; chargedProtocol(1.4) = floor(7·14000/10000) = 9
	//   extraFee = 9 − 7 = 2   (the CLP leg is m-independent, identical in both)
	// Under the OLD "m on full fee" policy this would have been ~41 (baseCLP·0.4).
	extraFee := outBase.UserOutput - outHigh.UserOutput
	if extraFee != 2 {
		t.Fatalf("extra fee from m=1.4 = %d, want 2 (m must ride the protocol leg only)", extraFee)
	}
	// Guard against regression back to the m-on-CLP blowup: baseCLP here is ~98,
	// so m leaking onto it would push extraFee into the tens.
	const protocolOnlyBound = 5
	if extraFee > protocolOnlyBound {
		t.Fatalf("extra fee %d > %d — m is leaking onto the CLP leg", extraFee, protocolOnlyBound)
	}
}

// incidentGeometry reproduces the 2026-06 incident snapshot: s ≈ 1.82, far above
// the s_eq = 1.0 target, which pins the stabilizer multiplier m at its 2× cap.
func incidentGeometry() pendulumoracle.GeometryOutputs {
	return pendulumoracle.GeometryOutputs{
		OK:   true,
		V:    1_820_000,
		P:    910_000,
		E:    1_000_000,
		T:    1_000_000,
		SBps: 18_217, // 1.8217
	}
}

// incidentArgs reconstructs the overcharged swap (80.861 HBD in → ~1511 HIVE out)
// from its logged slip x/(x+X) ≈ 3.16%.
func incidentArgs() wasm_context.PendulumSwapFeeArgs {
	return wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd",
		AssetOut: "hive",
		X:        80_861,
		XReserve: 2_474_000,
		YReserve: 47_755_000,
	}
}

// incidentBaseFees recomputes grossOut, baseCLP and baseProtocol with the same
// integer ops the applier uses, so the regression tests can reason about the
// pre-/post-fix fee without re-deriving the whole pipeline.
func incidentBaseFees() (grossOut, baseCLP, baseProtocol *big.Int) {
	a := incidentArgs()
	x := big.NewInt(a.X)
	xPlusX := big.NewInt(a.X + a.XReserve)
	y := big.NewInt(a.YReserve)
	grossOut = intmath.MulDivFloor(x, y, xPlusX)
	x2 := new(big.Int).Mul(x, x)
	denom2 := new(big.Int).Mul(xPlusX, xPlusX)
	baseCLP = intmath.MulDivFloor(x2, y, denom2)
	baseProtocol = intmath.MulDivFloor(grossOut, big.NewInt(8), big.NewInt(pendulum.BpsScale))
	return
}

// TestCLPScaleTamesLargeSwap is the regression test for the 2026-06 overcharge.
// On the real incident swap (m pinned at 2×), the default CLPScaleBps = 1/16 must
// keep the total fee in the ~0.36% band instead of the ~6.5% the old "full CLP ×
// m" code charged — an order-of-magnitude reduction — while staying under the 1%
// clamp.
func TestCLPScaleTamesLargeSwap(t *testing.T) {
	a, acc := newApplier(t, incidentGeometry(), []string{"contract:pool-1"})
	res := a.ApplySwapFees("contract:pool-1", "tx-incident", 100, incidentArgs(), acc.fn)
	if res.IsErr() {
		t.Fatalf("incident swap failed: %v", res)
	}
	out := res.Unwrap()

	// Sanity: this is the worst case the incident hit — m at the 2× cap.
	if out.MultiplierBps != pendulum.BpsScale*2 {
		t.Fatalf("expected m pinned at 2× cap, got %d bps", out.MultiplierBps)
	}

	grossOut, baseCLP, baseProtocol := incidentBaseFees()
	newFee := new(big.Int).Sub(grossOut, big.NewInt(out.UserOutput))

	// Bounded under the 1% clamp.
	feeCap := intmath.MulDivFloor(grossOut, big.NewInt(100), big.NewInt(pendulum.BpsScale))
	if newFee.Cmp(feeCap) > 0 {
		t.Fatalf("new fee %s exceeds 1%% clamp %s", newFee, feeCap)
	}

	// Pinned to the k=1/16 band: 0.30%–0.60% of grossOut (the model said 0.36%).
	// Lower bound proves the CLP leg still contributes (k≠0); upper bound proves
	// the overcharge is gone.
	lo := intmath.MulDivFloor(grossOut, big.NewInt(30), big.NewInt(pendulum.BpsScale))
	hi := intmath.MulDivFloor(grossOut, big.NewInt(60), big.NewInt(pendulum.BpsScale))
	if newFee.Cmp(lo) < 0 || newFee.Cmp(hi) > 0 {
		t.Fatalf("new fee %s outside the [%s, %s] (0.30–0.60%%) k=1/16 band", newFee, lo, hi)
	}

	// At least 10× cheaper than the old policy (baseCLP·m + baseProtocol·m).
	m := big.NewInt(int64(out.MultiplierBps))
	oldFee := new(big.Int).Add(
		intmath.MulDivFloor(baseCLP, m, big.NewInt(pendulum.BpsScale)),
		intmath.MulDivFloor(baseProtocol, m, big.NewInt(pendulum.BpsScale)),
	)
	if new(big.Int).Mul(newFee, big.NewInt(10)).Cmp(oldFee) >= 0 {
		t.Fatalf("new fee %s not ≥10× below old fee %s", newFee, oldFee)
	}
}

// TestMaxFeeClampBinds proves the backstop: even if CLPScaleBps is misconfigured
// all the way back to full slip (k=1), the clamp trims the CLP leg so the total
// fee lands exactly on grossOut·MaxFeeBps/10000, and the network/pendulum split
// still conserves against that capped total.
func TestMaxFeeClampBinds(t *testing.T) {
	cfg := DefaultConfig()
	cfg.CLPScaleBps = pendulum.BpsScale // 1.0 → full slip, reproduces the overcharge pre-clamp
	a := New(&stubGeometry{out: incidentGeometry()}, func() []string { return []string{"contract:pool-1"} }, cfg)
	acc := &recordingAccrual{}

	res := a.ApplySwapFees("contract:pool-1", "tx-clamp", 100, incidentArgs(), acc.fn)
	if res.IsErr() {
		t.Fatalf("clamped swap failed: %v", res)
	}
	out := res.Unwrap()

	grossOut, _, _ := incidentBaseFees()
	feeCap := intmath.MulDivFloor(grossOut, big.NewInt(int64(cfg.MaxFeeBps)), big.NewInt(pendulum.BpsScale))
	newFee := new(big.Int).Sub(grossOut, big.NewInt(out.UserOutput))
	if newFee.Cmp(feeCap) != 0 {
		t.Fatalf("clamp did not bind exactly: fee %s, cap %s", newFee, feeCap)
	}

	// Conservation against the capped total: lp + node + network == total fee.
	split := big.NewInt(out.LpShareOutput + out.NodeShareOutput + out.NetworkCreditOutput)
	if split.Cmp(feeCap) != 0 {
		t.Fatalf("splits %s do not conserve to capped fee %s", split, feeCap)
	}
}

// TestActivationHeightGate proves the rollout gate: below cfg.ActivationHeight
// the applier charges the legacy (overcharged) fee, and at/after it charges the
// bounded fix. Same incident swap and geometry — only the block height differs.
func TestActivationHeightGate(t *testing.T) {
	const H = 107_396_400
	cfg := DefaultConfig()
	cfg.ActivationHeight = H
	mk := func() *Applier {
		return New(&stubGeometry{out: incidentGeometry()}, func() []string { return []string{"contract:pool-1"} }, cfg)
	}
	grossOut, _, _ := incidentBaseFees()
	fee := func(bh uint64, tx string) *big.Int {
		res := mk().ApplySwapFees("contract:pool-1", tx, bh, incidentArgs(), (&recordingAccrual{}).fn)
		if res.IsErr() {
			t.Fatalf("swap at height %d failed: %v", bh, res)
		}
		return new(big.Int).Sub(grossOut, big.NewInt(res.Unwrap().UserOutput))
	}

	preFee := fee(H-1, "tx-pre") // one block before activation → legacy overcharge
	atFee := fee(H, "tx-at")     // activation block → bounded fix

	// Pre-activation must be in the overcharge regime (>5% of grossOut here).
	overchargeBar := intmath.MulDivFloor(grossOut, big.NewInt(500), big.NewInt(pendulum.BpsScale))
	if preFee.Cmp(overchargeBar) < 0 {
		t.Fatalf("pre-activation fee %s below the >5%% overcharge regime — gate not holding legacy math", preFee)
	}
	// Activation must cut the fee by at least 10×.
	if new(big.Int).Mul(atFee, big.NewInt(10)).Cmp(preFee) >= 0 {
		t.Fatalf("activation did not cut the fee ≥10×: pre=%s at=%s", preFee, atFee)
	}
	// And the post-activation fee must obey the 1% clamp.
	feeCap := intmath.MulDivFloor(grossOut, big.NewInt(100), big.NewInt(pendulum.BpsScale))
	if atFee.Cmp(feeCap) > 0 {
		t.Fatalf("post-activation fee %s exceeds 1%% clamp %s", atFee, feeCap)
	}
}

// TestAccrualErrorPropagates confirms that an error returned by the accrual
// callback aborts the swap with errAccrualFailed wrapping. This is the
// "insufficient HBD on the pool contract account" case in production —
// LedgerSession.ExecuteTransfer rejects the transfer and ApplySwapFees must
// not silently succeed.
func TestAccrualErrorPropagates(t *testing.T) {
	a, _ := newApplier(t, balancedGeometry(), []string{"contract:pool-1"})

	failingAccrual := func(amountHBD int64) error {
		return errBucketUnderfunded
	}

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args, failingAccrual)
	if !res.IsErr() {
		t.Fatal("expected error when accrual callback fails")
	}
}

// TestExacerbatesFromSnapshot pins the truth table for the auto-derived
// stabilizer hint: HBD-in raises s, HBD-out lowers it; "exacerbates"
// means the swap moves s away from the target s_eq = 1.0.
func TestExacerbatesFromSnapshot(t *testing.T) {
	target := pendulum.TargetSBps         // 1.0 in bps
	low := pendulum.BpsScale * 80 / 100   // 0.8 in bps (below target)
	high := pendulum.BpsScale * 120 / 100 // 1.2 in bps (above target)

	cases := []struct {
		name  string
		sBps  int64
		hbdIn bool
		want  bool
	}{
		{"s_low_hbd_in_corrective", low, true, false},
		{"s_low_hbd_out_exacerbates", low, false, true},
		{"s_high_hbd_in_exacerbates", high, true, true},
		{"s_high_hbd_out_corrective", high, false, false},
		{"s_at_target_hbd_in_exacerbates", target, true, true},
		{"s_at_target_hbd_out_exacerbates", target, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := exacerbatesFromSnapshot(tc.sBps, tc.hbdIn)
			if got != tc.want {
				t.Fatalf("got %v want %v (s=%d hbdIn=%v)", got, tc.want, tc.sBps, tc.hbdIn)
			}
		})
	}
}

// TestApplierConfiguredNilDeps exercises the defensive nil guard so a partly-
// wired state engine returns a clean error rather than panicking. Includes the
// nil accrual callback case.
func TestApplierConfiguredNilDeps(t *testing.T) {
	a := New(nil, nil, DefaultConfig())
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hbd", "hive"), nil)
	if !res.IsErr() {
		t.Fatal("expected error from nil-dep applier")
	}

	// Configured applier but nil accrual callback also fails cleanly.
	a2 := New(&stubGeometry{out: balancedGeometry()}, func() []string { return []string{"contract:pool-1"} }, DefaultConfig())
	res = a2.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hbd", "hive"), nil)
	if !res.IsErr() {
		t.Fatal("expected error when accrual callback is nil")
	}
}

// errBucketUnderfunded is a sentinel for the failing-accrual test; it stands
// in for the production "insufficient balance on contract HBD" failure that
// LedgerSession.ExecuteTransfer would return.
var errBucketUnderfunded = errSentinel("insufficient balance")

type errSentinel string

func (e errSentinel) Error() string { return string(e) }
