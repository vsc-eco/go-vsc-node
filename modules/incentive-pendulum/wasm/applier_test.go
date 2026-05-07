package pendulumwasm

import (
	"testing"

	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulum "vsc-node/modules/incentive-pendulum"
	wasm_context "vsc-node/modules/wasm/context"
)

// stubSnapshots is a deterministic SnapshotReader for unit tests.
type stubSnapshots struct {
	rec *pendulum_oracle.SnapshotRecord
}

func (s *stubSnapshots) GetSnapshotAtOrBefore(blockHeight uint64) (*pendulum_oracle.SnapshotRecord, bool, error) {
	if s.rec == nil {
		return nil, false, nil
	}
	return s.rec, true, nil
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

func balancedSnapshot() *pendulum_oracle.SnapshotRecord {
	// V = E and P = V/2: PDF equilibrium s = 1.0 (cliff to all-nodes), but we
	// use s strictly less than 1.0 so the proper split path executes.
	// Pick V=500_000, E=1_000_000, T=1_000_000, P=250_000 → s = V/E = 0.5.
	return &pendulum_oracle.SnapshotRecord{
		TickBlockHeight: 100,
		GeometryOK:      true,
		GeometryV:       500_000,
		GeometryP:       250_000,
		GeometryE:       1_000_000,
		GeometryT:       1_000_000,
		GeometrySBps:    pendulum.BpsScale / 2, // s = 0.5 → 5000 bps
	}
}

func newApplier(t *testing.T, snap *pendulum_oracle.SnapshotRecord, whitelist []string) (*Applier, *recordingAccrual) {
	t.Helper()
	a := New(
		&stubSnapshots{rec: snap},
		func() []string { return whitelist },
		Config{
			Stabilizer:      pendulum.DefaultStabilizerParamsBps(),
			NetworkShareNum: 1,
			NetworkShareDen: 4,
		},
	)
	return a, &recordingAccrual{}
}

// TestRejectsNonWhitelistedContract is the first guard the SDK method
// applies — calls from contracts not in the whitelist produce a clean error
// without invoking the accrual callback or snapshot DB.
func TestRejectsNonWhitelistedContract(t *testing.T) {
	a, acc := newApplier(t, balancedSnapshot(), []string{"contract:other"})
	res := a.ApplySwapFees("contract:not-whitelisted", "tx-1", 100, defaultArgs("hbd", "hive"), acc.fn)
	if !res.IsErr() {
		t.Fatal("expected error for non-whitelisted contract")
	}
	if len(acc.calls) != 0 {
		t.Fatalf("expected no accrual calls, got %d", len(acc.calls))
	}
}

// TestRejectsMissingSnapshot guards against pre-warmup swap calls — until W7
// populates geometry, GeometryOK == false should refuse the swap rather than
// silently mis-priced.
func TestRejectsMissingSnapshot(t *testing.T) {
	snap := balancedSnapshot()
	snap.GeometryOK = false
	a, acc := newApplier(t, snap, []string{"contract:pool-1"})
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hbd", "hive"), acc.fn)
	if !res.IsErr() {
		t.Fatal("expected error for missing snapshot")
	}
}

// TestRejectsNonHBDPair confirms the testnet-only HBD-paired requirement.
func TestRejectsNonHBDPair(t *testing.T) {
	a, acc := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})
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
	a, acc := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

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
	a, acc := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

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

// TestUnderSecuredCliffRoutesAllToNodes locks in the V≥E cliff: when the vault
// outweighs the bond, the entire pendulum pot routes to nodes per SplitInt.
func TestUnderSecuredCliffRoutesAllToNodes(t *testing.T) {
	snap := balancedSnapshot()
	snap.GeometryV = snap.GeometryE + 1 // V > E → cliff
	snap.GeometrySBps = pendulum.BpsScale * 11 / 10
	a, acc := newApplier(t, snap, []string{"contract:pool-1"})

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
	a, acc := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

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
	a, acc := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

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
	a, acc := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

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

// TestStabilizerMultiplierAppliesToFullFee pins the policy decision from
// review-and-plan.md issue #7: m rides on (baseProtocol + baseCLP), not
// protocol alone. The load-bearing assertion is that the extra fee charged
// when m > 1 is dominated by the CLP leg — under the prior "m on protocol
// only" code, the delta would be ~baseProtocol·(m−1) (single-digit base
// units in this scenario), nowhere near baseCLP·(m−1).
func TestStabilizerMultiplierAppliesToFullFee(t *testing.T) {
	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hbd", // HBD-in raises s; with s already > 0.5, this exacerbates → push=1.0.
		AssetOut: "hive",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}

	// Baseline: s = 0.5 → m = 1.0 → totalFee == baseCLP + baseProtocol.
	aBase, accBase := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})
	resBase := aBase.ApplySwapFees("contract:pool-1", "tx-1", 100, args, accBase.fn)
	if resBase.IsErr() {
		t.Fatalf("baseline swap failed: %v", resBase)
	}
	outBase := resBase.Unwrap()
	if outBase.MultiplierBps != pendulum.BpsScale {
		t.Fatalf("expected m == 1.0 at s=0.5, got %d bps", outBase.MultiplierBps)
	}

	// Off-equilibrium: s = 0.7 with V=700_000 (P=350_000=V/2 keeps geometry
	// consistent). At r = x/X = 1% and r0 = 1% → r/r0 = 1.0 → inner = 2.0.
	// tail = K · |s−0.5| · inner · push = 1 · 0.2 · 2 · 1 = 0.4 → m = 1.4.
	snapHigh := balancedSnapshot()
	snapHigh.GeometryV = 700_000
	snapHigh.GeometryP = 350_000
	snapHigh.GeometrySBps = pendulum.BpsScale * 70 / 100
	aHigh, accHigh := newApplier(t, snapHigh, []string{"contract:pool-1"})
	resHigh := aHigh.ApplySwapFees("contract:pool-1", "tx-2", 100, args, accHigh.fn)
	if resHigh.IsErr() {
		t.Fatalf("off-equilibrium swap failed: %v", resHigh)
	}
	outHigh := resHigh.Unwrap()
	if outHigh.MultiplierBps != pendulum.BpsScale*14/10 {
		t.Fatalf("expected m == 1.4 at s=0.7, r=1%%, got %d bps", outHigh.MultiplierBps)
	}

	// Reserves identical in both runs → grossOut, baseCLP, baseProtocol all
	// identical. Any difference in userOutput is the extra fee charged by m.
	extraFee := outBase.UserOutput - outHigh.UserOutput
	if extraFee <= 0 {
		t.Fatalf("expected user output to drop with m>1, got base=%d high=%d", outBase.UserOutput, outHigh.UserOutput)
	}

	// Hand-computed at args + s=0.7:
	//   grossOut       = floor(10000·1_000_000 / 1_010_000) = 9900
	//   baseCLP        = floor(10000² · 1_000_000 / 1_010_000²) = 98
	//   baseProtocol   = floor(9900 · 8 / 10000) = 7
	//   chargedCLP     = floor(98 · 14000 / 10000) = 137 → surplusCLP = 39
	//   chargedProtocol= floor(7 · 14000 / 10000) = 9   → surplusProtocol = 2
	// Total surplus under "m on full fee" = 41.
	// Under prior "m on protocol only" the surplus would have been just 2.
	wantExtraFee := int64(41)
	if extraFee != wantExtraFee {
		t.Fatalf("extra fee from m=1.4 = %d, want %d (m must ride on full fee, not protocol only)", extraFee, wantExtraFee)
	}

	// Sanity: the CLP leg alone contributes far more than the protocol leg —
	// i.e., even if integer rounding shifts the exact value, the delta cannot
	// collapse back to the "protocol-only" regime.
	const protocolOnlyBound = 5 // generous upper bound on (chargedProtocol − baseProtocol)
	if extraFee <= protocolOnlyBound {
		t.Fatalf("extra fee %d ≤ protocol-only bound %d — CLP leg is not being multiplied", extraFee, protocolOnlyBound)
	}
}

// TestAccrualErrorPropagates confirms that an error returned by the accrual
// callback aborts the swap with errAccrualFailed wrapping. This is the
// "insufficient HBD on the pool contract account" case in production —
// LedgerSession.ExecuteTransfer rejects the transfer and ApplySwapFees must
// not silently succeed.
func TestAccrualErrorPropagates(t *testing.T) {
	a, _ := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

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
// means the swap moves s away from 0.5.
func TestExacerbatesFromSnapshot(t *testing.T) {
	half := pendulum.BpsScale / 2          // 0.5 in bps
	low := pendulum.BpsScale * 30 / 100    // 0.3 in bps
	high := pendulum.BpsScale * 70 / 100   // 0.7 in bps

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
		{"s_at_half_hbd_in_exacerbates", half, true, true},
		{"s_at_half_hbd_out_exacerbates", half, false, true},
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
	a2 := New(&stubSnapshots{rec: balancedSnapshot()}, func() []string { return []string{"contract:pool-1"} }, DefaultConfig())
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
