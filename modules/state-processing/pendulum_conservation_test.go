package state_engine_test

import (
	"errors"
	"testing"

	"vsc-node/lib/intmath"
	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
	pendulum "vsc-node/modules/incentive-pendulum"
	pendulumwasm "vsc-node/modules/incentive-pendulum/wasm"
	ledgerSystem "vsc-node/modules/ledger-system"
	wasm_context "vsc-node/modules/wasm/context"
)

// stubSnapshotsForConservation is a deterministic SnapshotReader for the
// HBD-conservation tests. Mirrors the in-package stub used by the applier's
// own tests; duplicated here because the wasm package can't import test_utils
// without an import cycle.
type stubSnapshotsForConservation struct {
	rec *pendulum_oracle.SnapshotRecord
}

func (s *stubSnapshotsForConservation) GetSnapshotAtOrBefore(_ uint64) (*pendulum_oracle.SnapshotRecord, bool, error) {
	if s.rec == nil {
		return nil, false, nil
	}
	return s.rec, true, nil
}

func balancedConservationSnapshot() *pendulum_oracle.SnapshotRecord {
	return &pendulum_oracle.SnapshotRecord{
		TickBlockHeight: 100,
		GeometryOK:      true,
		GeometryV:       500_000,
		GeometryP:       250_000,
		GeometryE:       1_000_000,
		GeometryT:       1_000_000,
		GeometrySBps:    intmath.BpsScale / 2,
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
	ls := ledgerSystem.New(balDb, lDb, nil, aDb)
	state := ls.NewEmptyState()
	session := ledgerSystem.NewSession(state)

	a := pendulumwasm.New(
		&stubSnapshotsForConservation{rec: balancedConservationSnapshot()},
		func() []string { return []string{contractID} },
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
	ls := ledgerSystem.New(balDb, lDb, nil, aDb)
	state := ls.NewEmptyState()
	session := ledgerSystem.NewSession(state)

	a := pendulumwasm.New(
		&stubSnapshotsForConservation{rec: balancedConservationSnapshot()},
		func() []string { return []string{contractID} },
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
