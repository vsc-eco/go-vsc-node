package state_engine_test

import (
	"errors"
	"testing"

	"vsc-node/lib/intmath"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/consensusversion"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulum "vsc-node/modules/incentive-pendulum"
	pendulumoracle "vsc-node/modules/incentive-pendulum/oracle"
	pendulumwasm "vsc-node/modules/incentive-pendulum/wasm"
	ledgerSystem "vsc-node/modules/ledger-system"
	wasm_context "vsc-node/modules/wasm/context"
)

// stubGeometryForConservation is a deterministic GeometryReader for the
// HBD-conservation tests. Mirrors the applier package's own stub; duplicated
// here so this _test package can run without importing the wasm package's
// internal test helpers.
type stubGeometryForConservation struct {
	out pendulumoracle.GeometryOutputs
}

func (s *stubGeometryForConservation) GeometryAt(_ uint64) (pendulumoracle.GeometryOutputs, bool) {
	return s.out, s.out.OK
}

func balancedConservationGeometry() pendulumoracle.GeometryOutputs {
	return pendulumoracle.GeometryOutputs{
		OK:   true,
		V:    500_000,
		P:    250_000,
		E:    1_000_000,
		T:    1_000_000,
		SBps: intmath.BpsScale / 2,
	}
}

// realSessionAccrual builds an AccrueNodeBucketFn backed by a real
// LedgerSession. The accrual goes through the production ExecuteTransfer
// code path: paired (debit, credit) ledger ops with type=transfer, with the
// source-balance check enforced. Id matches the convention used by
// SendBalance/PullBalance — pass the bare TxId; the session's idCache
// disambiguates multiple ExecuteTransfer calls under the same TxId.
func realSessionAccrual(contractID string, txID string, blockHeight uint64, session ledgerSystem.LedgerSession) wasm_context.AccrueNodeBucketFn {
	return func(amountHBD int64) error {
		if amountHBD <= 0 {
			return nil
		}
		res := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
			From:        "contract:" + contractID,
			To:          ledgerSystem.PendulumNodesHBDBucket,
			Amount:      amountHBD,
			Asset:       "hbd",
			Type:        "transfer",
			Id:          txID,
			BlockHeight: blockHeight,
		})
		if !res.Ok {
			return errors.New(res.Msg)
		}
		return nil
	}
}

// TestPendulumAccrualHBDConservation exercises the end-to-end accrual flow
// against a real LedgerSession + LedgerState — proving the closing of issue
// #2: the bucket is credited via a paired transfer, the contract account is
// debited by the same amount, and total HBD across all accounts is exactly
// preserved.
//
// Pre-fix this assertion would fail by `nodeBucketHBD` units per swap (the
// old PendulumAccrue minted bucket HBD with no offsetting debit).
func TestPendulumAccrualHBDConservation(t *testing.T) {
	contractID := "pool-1"
	contractAcct := "contract:" + contractID

	const seededY = int64(1_000_000)
	balDb := &test_utils.MockBalanceDb{
		BalanceRecords: map[string][]ledgerDb.BalanceRecord{
			contractAcct: {{
				Account:     contractAcct,
				BlockHeight: 99,
				HBD:         seededY,
			}},
		},
	}
	lDb := &test_utils.MockLedgerDb{
		LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
	}
	aDb := &test_utils.MockActionsDb{
		Actions: make(map[string]ledgerDb.ActionRecord),
	}
	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)
	state := ls.NewEmptyState()
	session := ledgerSystem.NewSession(state)

	a := pendulumwasm.New(
		&stubGeometryForConservation{out: balancedConservationGeometry()},
		func() []string { return []string{contractID} },
		nil, // LP floor inert in conservation tests
		pendulumwasm.Config{
			Stabilizer:      pendulum.DefaultStabilizerParamsBps(),
			NetworkShareNum: 1,
			NetworkShareDen: 4,
		},
	)

	const swapBh = uint64(100)
	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: seededY,
	}
	res := a.ApplySwapFees(contractID, "tx-conservation", swapBh, args, realSessionAccrual(contractID, "tx-conservation", swapBh, session))
	if res.IsErr() {
		t.Fatalf("ApplySwapFees: %v", res.UnwrapErr())
	}
	out := res.Unwrap()
	if out.NodeBucketCreditedHBD <= 0 {
		t.Fatalf("expected positive accrual, got %d", out.NodeBucketCreditedHBD)
	}

	// Commit the session so the ledger ops materialize on state.VirtualLedger.
	session.Done()

	// SnapshotForAccount sums the seeded balance record + virtual ledger ops
	// — both sides of the paired transfer are visible here. (PendulumBucketBalance
	// reads from the underlying LedgerDb, which the in-memory session hasn't
	// flushed to.)
	contractBal := state.SnapshotForAccount(contractAcct, swapBh, "hbd")
	bucketBal := state.SnapshotForAccount(ledgerSystem.PendulumNodesHBDBucket, swapBh, "hbd")

	contractDelta := contractBal - seededY
	if contractDelta != -out.NodeBucketCreditedHBD {
		t.Fatalf("contract HBD delta %d != -%d (expected paired debit)", contractDelta, out.NodeBucketCreditedHBD)
	}
	if bucketBal != out.NodeBucketCreditedHBD {
		t.Fatalf("bucket HBD %d != reported credit %d", bucketBal, out.NodeBucketCreditedHBD)
	}
	// Total HBD in the system is exactly preserved across the two accounts.
	if contractBal+bucketBal != seededY {
		t.Fatalf("HBD conservation broken: contract=%d bucket=%d sum=%d (want %d)",
			contractBal, bucketBal, contractBal+bucketBal, seededY)
	}
}

// TestPendulumAccrualFailsWhenContractUnderfunded confirms that
// ExecuteTransfer's insufficient-balance guard rejects the swap when the pool
// contract's HBD balance can't cover the node-bucket accrual. Pre-fix the old
// minting accrual would have silently created HBD; under the new design the
// swap aborts.
func TestPendulumAccrualFailsWhenContractUnderfunded(t *testing.T) {
	contractID := "pool-2"
	contractAcct := "contract:" + contractID

	// Seed the contract with much less HBD than the swap math says lives in
	// the pool's Y reserve. The accrual will demand HBD the contract doesn't
	// actually have, and ExecuteTransfer must refuse.
	balDb := &test_utils.MockBalanceDb{
		BalanceRecords: map[string][]ledgerDb.BalanceRecord{
			contractAcct: {{
				Account:     contractAcct,
				BlockHeight: 99,
				HBD:         5, // far below YReserve
			}},
		},
	}
	lDb := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	aDb := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)
	state := ls.NewEmptyState()
	session := ledgerSystem.NewSession(state)

	a := pendulumwasm.New(
		&stubGeometryForConservation{out: balancedConservationGeometry()},
		func() []string { return []string{contractID} },
		nil, // LP floor inert in conservation tests
		pendulumwasm.Config{
			Stabilizer:      pendulum.DefaultStabilizerParamsBps(),
			NetworkShareNum: 1,
			NetworkShareDen: 4,
		},
	)

	args := wasm_context.PendulumSwapFeeArgs{
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        10_000,
		XReserve: 1_000_000,
		YReserve: 1_000_000,
	}
	res := a.ApplySwapFees(contractID, "tx-underfunded", 100, args, realSessionAccrual(contractID, "tx-underfunded", 100, session))
	if !res.IsErr() {
		t.Fatal("expected swap to fail when contract HBD is insufficient for the accrual")
	}
}

// underSecuredConservationGeometry is the cliff regime the floor targets:
// V = 4_000_000 >= c*E = 3*1_000_000, where the raw pendulum routes 100% to
// nodes. (SBps = 4.0 in bps.)
func underSecuredConservationGeometry() pendulumoracle.GeometryOutputs {
	return pendulumoracle.GeometryOutputs{
		OK: true, V: 4_000_000, P: 2_000_000, E: 1_000_000, T: 1_000_000,
		SBps: 4 * intmath.BpsScale,
	}
}

// TestPendulumLPFloorBindsUnderSecured drives a real swap through the
// production LedgerSession on an UNDER-SECURED pool and proves the
// consensus-0.2.0 LP minimum-floor binds end-to-end through the ledger, not
// just in the pure-applier unit test:
//
//   - With the chain below 0.2.0 the under-secured cliff still routes 100% of
//     the pot to nodes (LP share 0) — the historical behavior.
//   - At 0.2.0 the node share is capped at 75% (DefaultLPFloorBps = 25% LP
//     floor): LPs keep ~25% of the pot, the node bucket is credited only the
//     capped amount, and HBD is conserved across the paired transfer.
//
// Running both sides with identical geometry/args isolates the difference to
// the floor (consensus gate), not the swap math.
func TestPendulumLPFloorBindsUnderSecured(t *testing.T) {
	const seededY = int64(100_000_000)
	args := wasm_context.PendulumSwapFeeArgs{
		// HBD-out swap: the node share drains directly as HBD (no secondary
		// hop), keeping the conservation assertion simple.
		AssetIn:  "hive",
		AssetOut: "hbd",
		X:        100_000,
		XReserve: 1_000_000,
		YReserve: seededY,
	}

	// run executes one swap end-to-end through a real LedgerSession at the given
	// chain-active consensus version, asserts HBD conservation, and returns the
	// swap result.
	run := func(t *testing.T, consensus uint64) wasm_context.PendulumSwapFeeResult {
		t.Helper()
		contractID := "pool-floor"
		contractAcct := "contract:" + contractID
		balDb := &test_utils.MockBalanceDb{
			BalanceRecords: map[string][]ledgerDb.BalanceRecord{
				contractAcct: {{Account: contractAcct, BlockHeight: 99, HBD: seededY}},
			},
		}
		lDb := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
		aDb := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
		ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)
		state := ls.NewEmptyState()
		session := ledgerSystem.NewSession(state)

		a := pendulumwasm.New(
			&stubGeometryForConservation{out: underSecuredConservationGeometry()},
			func() []string { return []string{contractID} },
			func(uint64) consensusversion.Version {
				return consensusversion.Version{Major: 0, Consensus: consensus}
			},
			pendulumwasm.DefaultConfig(), // MinFractionBps = DefaultLPFloorBps (2500 = 25%)
		)

		const swapBh = uint64(100)
		res := a.ApplySwapFees(contractID, "tx-floor", swapBh, args, realSessionAccrual(contractID, "tx-floor", swapBh, session))
		if res.IsErr() {
			t.Fatalf("ApplySwapFees (consensus 0.%d): %v", consensus, res.UnwrapErr())
		}
		out := res.Unwrap()
		session.Done()

		// HBD conservation across the paired transfer holds regardless of the floor.
		contractBal := state.SnapshotForAccount(contractAcct, swapBh, "hbd")
		bucketBal := state.SnapshotForAccount(ledgerSystem.PendulumNodesHBDBucket, swapBh, "hbd")
		if bucketBal != out.NodeBucketCreditedHBD {
			t.Fatalf("consensus 0.%d: bucket HBD %d != reported credit %d", consensus, bucketBal, out.NodeBucketCreditedHBD)
		}
		if contractBal+bucketBal != seededY {
			t.Fatalf("consensus 0.%d: HBD conservation broken: contract=%d bucket=%d sum=%d (want %d)",
				consensus, contractBal, bucketBal, contractBal+bucketBal, seededY)
		}
		return out
	}

	floorOn := run(t, 2)  // >= LPFloorActivation (0.2.0) → floor enforced
	floorOff := run(t, 1) // 0.1.0, below activation → historical cliff

	// Pre-activation: the under-secured cliff routes the entire pot to nodes.
	if floorOff.LpShareOutput != 0 {
		t.Fatalf("pre-0.2.0 under-secured swap must route 100%% to nodes, got LP share %d", floorOff.LpShareOutput)
	}

	// The floor only redistributes the pot between LP and node — pot size and
	// the network cut are untouched.
	pot := floorOff.LpShareOutput + floorOff.NodeShareOutput
	if pot <= 0 {
		t.Fatalf("degenerate test: empty pendulum pot")
	}
	if floorOn.LpShareOutput+floorOn.NodeShareOutput != pot {
		t.Fatalf("floor resized the pot: on=%d off=%d", floorOn.LpShareOutput+floorOn.NodeShareOutput, pot)
	}
	if floorOn.NetworkCreditOutput != floorOff.NetworkCreditOutput {
		t.Fatalf("floor changed the network cut: on=%d off=%d", floorOn.NetworkCreditOutput, floorOff.NetworkCreditOutput)
	}

	// At 0.2.0 LPs keep ~25% of the pot and nodes are capped at ~75% (the two
	// pots floor-divide independently, so allow a couple base units of slack).
	quarter := pot / 4
	if floorOn.LpShareOutput < quarter-2 {
		t.Fatalf("LP floor not enforced: LP share %d < ~25%% of pot %d", floorOn.LpShareOutput, pot)
	}
	if floorOn.NodeShareOutput > pot-quarter+2 {
		t.Fatalf("node share %d exceeds ~75%% cap of pot %d", floorOn.NodeShareOutput, pot)
	}
	// The floor moved real money out of the node bucket and into LP reserves.
	if floorOn.NodeBucketCreditedHBD >= floorOff.NodeBucketCreditedHBD {
		t.Fatalf("floor should reduce node-bucket accrual: on=%d off=%d",
			floorOn.NodeBucketCreditedHBD, floorOff.NodeBucketCreditedHBD)
	}
	t.Logf("under-secured pot=%d: floor-off node=%d/LP=%d → floor-on node=%d/LP=%d (bucket %d→%d)",
		pot, floorOff.NodeShareOutput, floorOff.LpShareOutput,
		floorOn.NodeShareOutput, floorOn.LpShareOutput,
		floorOff.NodeBucketCreditedHBD, floorOn.NodeBucketCreditedHBD)
}
