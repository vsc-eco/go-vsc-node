package pendulumwasm

import (
	"testing"

	"vsc-node/lib/intmath"
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulum "vsc-node/modules/incentive-pendulum"
	ledgerSystem "vsc-node/modules/ledger-system"
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

// recordingLedger captures every PendulumAccrue call so tests can assert on
// the bucket movement without standing up MongoDB.
type recordingLedger struct {
	calls []recordedAccrual
}

type recordedAccrual struct {
	Account string
	Asset   string
	Amount  int64
	TxID    string
	Height  uint64
}

func (r *recordingLedger) PendulumAccrue(account, asset string, amount int64, txID string, blockHeight uint64) ledgerSystem.LedgerResult {
	r.calls = append(r.calls, recordedAccrual{
		Account: account, Asset: asset, Amount: amount, TxID: txID, Height: blockHeight,
	})
	return ledgerSystem.LedgerResult{Ok: true}
}

func defaultArgs(assetIn, assetOut string) wasm_context.PendulumSwapFeeArgs {
	return wasm_context.PendulumSwapFeeArgs{
		AssetIn:     assetIn,
		AssetOut:    assetOut,
		X:           1_000,        // 0.001 of input asset (small swap)
		XReserve:    1_000_000,    // 1.0 input asset reserve
		YReserve:    1_000_000,    // 1.0 output asset reserve
		BaseCLP:     1,            // contract-supplied CLP fee
		Exacerbates: false,
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
		GeometryS:       intmath.SQ64Scale / 2,
	}
}

func newApplier(t *testing.T, snap *pendulum_oracle.SnapshotRecord, whitelist []string) (*Applier, *recordingLedger) {
	t.Helper()
	led := &recordingLedger{}
	a := New(
		&stubSnapshots{rec: snap},
		led,
		func() []string { return whitelist },
		Config{
			Stabilizer:      pendulum.DefaultStabilizerParamsFixed(),
			NetworkShareNum: 1,
			NetworkShareDen: 4,
		},
	)
	return a, led
}

// TestRejectsNonWhitelistedContract is the first guard the SDK method
// applies — calls from contracts not in the whitelist produce a clean error
// without touching the ledger or snapshot DB.
func TestRejectsNonWhitelistedContract(t *testing.T) {
	a, led := newApplier(t, balancedSnapshot(), []string{"contract:other"})
	res := a.ApplySwapFees("contract:not-whitelisted", "tx-1", 100, defaultArgs("hbd", "hive"))
	if !res.IsErr() {
		t.Fatal("expected error for non-whitelisted contract")
	}
	if len(led.calls) != 0 {
		t.Fatalf("expected no ledger calls, got %d", len(led.calls))
	}
}

// TestRejectsMissingSnapshot guards against pre-warmup swap calls — until W7
// populates geometry, GeometryOK == false should refuse the swap rather than
// silently mis-priced.
func TestRejectsMissingSnapshot(t *testing.T) {
	snap := balancedSnapshot()
	snap.GeometryOK = false
	a, _ := newApplier(t, snap, []string{"contract:pool-1"})
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hbd", "hive"))
	if !res.IsErr() {
		t.Fatal("expected error for missing snapshot")
	}
}

// TestRejectsNonHBDPair confirms the testnet-only HBD-paired requirement.
func TestRejectsNonHBDPair(t *testing.T) {
	a, _ := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hive", "btc"))
	if !res.IsErr() {
		t.Fatal("expected error for non-HBD-paired swap")
	}
}

// TestSwapHBDInAccruesNodeBucket exercises a HBD→ASSET1 swap end to end:
// the protocol leg passes through (HBD), the CLP leg converts (ASSET1 → HBD),
// and the resulting node-share lands in pendulum:nodes:HBD.
func TestSwapHBDInAccruesNodeBucket(t *testing.T) {
	a, led := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:     "hbd",
		AssetOut:    "hive",
		X:           10_000,
		XReserve:    1_000_000,
		YReserve:    1_000_000,
		BaseCLP:     98, // ~ 10000^2 * 1000000 / 1010000^2 ≈ 98
		Exacerbates: true,
	}

	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}

	out := res.Unwrap()
	if out.NodeBucketCreditedHBD <= 0 {
		t.Fatalf("expected positive node bucket credit, got %d", out.NodeBucketCreditedHBD)
	}
	if len(led.calls) != 1 {
		t.Fatalf("expected exactly one ledger accrual, got %d", len(led.calls))
	}
	got := led.calls[0]
	if got.Account != "nodes" || got.Asset != "hbd" {
		t.Fatalf("ledger account/asset wrong: %+v", got)
	}
	if got.Amount != out.NodeBucketCreditedHBD {
		t.Fatalf("ledger amount %d != reported credit %d", got.Amount, out.NodeBucketCreditedHBD)
	}
	if out.UserOutput <= 0 {
		t.Fatalf("expected positive user output, got %d", out.UserOutput)
	}
	if out.NewXReserve <= 0 || out.NewYReserve <= 0 {
		t.Fatalf("expected positive new reserves, got X=%d Y=%d", out.NewXReserve, out.NewYReserve)
	}
}

// TestSwapASSET1InAccruesNodeBucket runs the mirror direction: ASSET1→HBD.
// The protocol leg now needs the secondary CPMM hop, the CLP leg passes through.
func TestSwapASSET1InAccruesNodeBucket(t *testing.T) {
	a, led := newApplier(t, balancedSnapshot(), []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:     "hive",
		AssetOut:    "hbd",
		X:           10_000,
		XReserve:    1_000_000,
		YReserve:    1_000_000,
		BaseCLP:     98,
		Exacerbates: true,
	}

	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}
	out := res.Unwrap()
	if out.NodeBucketCreditedHBD <= 0 {
		t.Fatalf("expected positive node bucket credit, got %d", out.NodeBucketCreditedHBD)
	}
	if len(led.calls) != 1 {
		t.Fatalf("expected one ledger call, got %d", len(led.calls))
	}
}

// TestUnderSecuredCliffRoutesAllToNodes locks in the V≥E cliff: when the vault
// outweighs the bond, the entire pendulum pot routes to nodes per SplitInt.
func TestUnderSecuredCliffRoutesAllToNodes(t *testing.T) {
	snap := balancedSnapshot()
	snap.GeometryV = snap.GeometryE + 1 // V > E → cliff
	snap.GeometryS = intmath.SQ64Scale * 11 / 10
	a, led := newApplier(t, snap, []string{"contract:pool-1"})

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:     "hbd",
		AssetOut:    "hive",
		X:           10_000,
		XReserve:    1_000_000,
		YReserve:    1_000_000,
		BaseCLP:     98,
		Exacerbates: true,
	}

	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, args)
	if res.IsErr() {
		t.Fatalf("expected success, got %v", res)
	}
	if len(led.calls) != 1 {
		t.Fatalf("expected one ledger call, got %d", len(led.calls))
	}
	// Cliff: f_node = 1, so all of the pendulum pot goes to nodes. The accrued
	// amount should be greater than zero and roughly proportional to 75% of
	// total fees (the network 25% stays in pool reserves).
	if led.calls[0].Amount <= 0 {
		t.Fatalf("expected positive node accrual under cliff, got %d", led.calls[0].Amount)
	}
}

// TestApplierConfiguredNilDeps exercises the defensive nil guard so a partly-
// wired state engine returns a clean error rather than panicking.
func TestApplierConfiguredNilDeps(t *testing.T) {
	a := New(nil, nil, nil, DefaultConfig())
	res := a.ApplySwapFees("contract:pool-1", "tx-1", 100, defaultArgs("hbd", "hive"))
	if !res.IsErr() {
		t.Fatal("expected error from nil-dep applier")
	}
}
