package state_engine_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// ============================================================
// TEST HELPERS for HP staking tests
// ============================================================

func newHPSession(balances map[string][]ledgerDb.BalanceRecord) ledgerSystem.LedgerSession {
	state := &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     1,
		LedgerDb:        newMockLedgerDb(),
		BalanceDb:       newMockBalanceDb(balances),
		ActionDb:        newMockActionsDb(),
	}
	return ledgerSystem.NewSession(state)
}

// Standard validator: 60K HIVE in consensus, 10K liquid HIVE, 5K HBD
func validatorBalances() map[string][]ledgerDb.BalanceRecord {
	return map[string][]ledgerDb.BalanceRecord{
		"hive:validator1": {{
			Account:        "hive:validator1",
			BlockHeight:    0,
			Hive:           10_000_000,
			HIVE_CONSENSUS: 60_000_000,
			HBD:            5_000_000,
			HIVE_HP:        0,
		}},
	}
}

// Validator with existing HP stake
func validatorWithHP() map[string][]ledgerDb.BalanceRecord {
	return map[string][]ledgerDb.BalanceRecord{
		"hive:validator1": {{
			Account:        "hive:validator1",
			BlockHeight:    0,
			Hive:           5_000_000,
			HIVE_CONSENSUS: 10_000_000,
			HBD:            5_000_000,
			HIVE_HP:        50_000_000,
		}},
	}
}

// ============================================================
// Phase 2: ASSET TYPE TESTS
// ============================================================

func TestHiveHPAssetTypeExists(t *testing.T) {
	session := newHPSession(validatorBalances())
	bal := session.GetBalance("hive:validator1", 1, "hive_hp")
	require.Equal(t, int64(0), bal, "new account should have 0 hive_hp")
}

func TestHiveHPBalanceRecordField(t *testing.T) {
	session := newHPSession(validatorWithHP())
	bal := session.GetBalance("hive:validator1", 1, "hive_hp")
	require.Equal(t, int64(50_000_000), bal, "should read HIVE_HP from balance record")
}

func TestHiveHPNotTransferable(t *testing.T) {
	session := newHPSession(validatorWithHP())
	result := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "transfer-hp-1",
		From:        "hive:validator1",
		To:          "hive:someone",
		Asset:       "hive_hp",
		Amount:      1_000,
		BlockHeight: 1,
	})
	require.False(t, result.Ok, "hive_hp transfer should be rejected")
	require.Equal(t, "invalid asset", result.Msg)
}

// ============================================================
// Phase 2+3: LEDGER SESSION TESTS for OptInHP / OptOutHP
// ============================================================

func TestOptInHP(t *testing.T) {
	session := newHPSession(validatorBalances())

	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-1",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      50_000_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})

	require.True(t, result.Ok, "opt-in should succeed with 50K+ from 60K consensus: %s", result.Msg)

	consensusBal := session.GetBalance("hive:validator1", 1, "hive_consensus")
	pendingBal := session.GetBalance("hive:validator1", 1, "pending_hp")
	hpBal := session.GetBalance("hive:validator1", 1, "hive_hp")

	require.Equal(t, int64(10_000_000), consensusBal, "consensus should be 60K - 50K = 10K")
	require.Equal(t, int64(50_000_000), pendingBal, "pending_hp should be 50K (two-phase)")
	require.Equal(t, int64(0), hpBal, "hive_hp should be 0 (not confirmed yet)")
}

func TestOptInHPMinimum50K(t *testing.T) {
	session := newHPSession(validatorBalances())

	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-low",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      49_999_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})

	require.False(t, result.Ok, "opt-in with < 50K should fail")
	require.Contains(t, result.Msg, "minimum")
}

func TestOptInHPInsufficientConsensus(t *testing.T) {
	session := newHPSession(validatorBalances())

	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-too-much",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      70_000_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})

	require.False(t, result.Ok, "opt-in exceeding consensus balance should fail")
	require.Equal(t, "insufficient balance", result.Msg)
}

func TestOptInHPMustKeepMinStake(t *testing.T) {
	session := newHPSession(validatorBalances())

	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-max",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      59_000_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})

	require.True(t, result.Ok, "should succeed since total stake still meets minimum: %s", result.Msg)
}

func TestOptOutHPBlocked(t *testing.T) {
	// HP_UNSTAKE_ENABLED is false until Phase 5 (fill_vesting_withdraw) ships.
	// All opt-outs should be rejected to prevent fund loss.
	session := newHPSession(validatorWithHP())

	result := session.OptOutHP(ledgerSystem.HPStakeParams{
		Id:            "opt-out-1",
		From:          "hive:validator1",
		To:            "hive:validator1",
		Amount:        50_000_000,
		BlockHeight:   1,
		ElectionEpoch: 10,
		HiveAccount:   "validator1",
	})

	require.False(t, result.Ok, "opt-out should be blocked until Phase 5 ships")
	require.Contains(t, result.Msg, "not yet enabled")
}

// ============================================================
// DOUBLE SPEND PREVENTION
// ============================================================

func TestOptInHPDoubleSpend(t *testing.T) {
	session := newHPSession(validatorBalances())

	result1 := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-1",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      50_000_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})
	require.True(t, result1.Ok)

	result2 := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-2",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      20_000_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})
	require.False(t, result2.Ok, "second opt-in should fail: insufficient consensus")
}

// ============================================================
// REGRESSION — existing operations still work after HP changes
// ============================================================

func TestRegressionConsensusStakeAfterHPChanges(t *testing.T) {
	session := newHPSession(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    0,
			Hive:           10_000,
			HIVE_CONSENSUS: 10_000,
			HBD:            10_000,
			HBD_SAVINGS:    10_000,
		}},
	})

	result := session.ConsensusStake(ledgerSystem.ConsensusParams{
		Id:          "cs-regression-1",
		From:        "hive:alice",
		To:          "hive:alice",
		Amount:      5_000,
		BlockHeight: 1,
		Type:        "stake",
	})
	require.True(t, result.Ok, "consensus stake should still work after HP changes")
}

func TestRegressionHBDStakeAfterHPChanges(t *testing.T) {
	session := newHPSession(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:     "hive:alice",
			BlockHeight: 0,
			HBD:         10_000,
		}},
	})

	result := session.Stake(ledgerSystem.StakeOp{
		OpLogEvent: ledgerSystem.OpLogEvent{
			Id:          "hbd-stake-regression",
			From:        "hive:alice",
			To:          "hive:alice",
			Asset:       "hbd",
			Amount:      5_000,
			BlockHeight: 1,
		},
	})
	require.True(t, result.Ok, "HBD stake should still work after HP changes")
}

func TestRegressionTransferAfterHPChanges(t *testing.T) {
	session := newHPSession(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:     "hive:alice",
			BlockHeight: 0,
			Hive:        10_000,
		}},
	})

	result := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "transfer-regression",
		From:        "hive:alice",
		To:          "hive:bob",
		Asset:       "hive",
		Amount:      5_000,
		BlockHeight: 1,
	})
	require.True(t, result.Ok, "HIVE transfer should still work after HP changes")
}

// ============================================================
// BUG F: Verify amount=0 is rejected
// ============================================================

func TestOptInHPZeroAmount(t *testing.T) {
	session := newHPSession(validatorBalances())

	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-zero",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      0,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})

	require.False(t, result.Ok, "opt-in with amount=0 should fail")
	require.Equal(t, "invalid amount", result.Msg)
}

// ============================================================
// BUG G: Test From != To (staking from one validator to another)
// ============================================================

func TestOptInHPDifferentFromTo(t *testing.T) {
	balances := map[string][]ledgerDb.BalanceRecord{
		"hive:validator1": {{
			Account:        "hive:validator1",
			BlockHeight:    0,
			Hive:           10_000_000,
			HIVE_CONSENSUS: 60_000_000,
			HBD:            5_000_000,
			HIVE_HP:        0,
		}},
		"hive:validator2": {{
			Account:        "hive:validator2",
			BlockHeight:    0,
			Hive:           0,
			HIVE_CONSENSUS: 0,
			HBD:            0,
			HIVE_HP:        0,
		}},
	}
	session := newHPSession(balances)

	// Stake from validator1's consensus into validator2's hive_hp
	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-cross",
		From:        "hive:validator1",
		To:          "hive:validator2",
		Amount:      50_000_000,
		BlockHeight: 1,
		HiveAccount: "validator2",
	})

	require.True(t, result.Ok, "cross-account opt-in should succeed: %s", result.Msg)

	// validator1's consensus should decrease
	v1Consensus := session.GetBalance("hive:validator1", 1, "hive_consensus")
	require.Equal(t, int64(10_000_000), v1Consensus, "validator1 consensus should drop by 50K")

	// validator2's pending_hp should increase (two-phase: not confirmed yet)
	v2PendingHP := session.GetBalance("hive:validator2", 1, "pending_hp")
	require.Equal(t, int64(50_000_000), v2PendingHP, "validator2 should receive 50K pending_hp")
}

// ============================================================
// BUG H: OptOut immediately after OptIn in same session
// ============================================================

func TestOptInThenOutSameSession(t *testing.T) {
	session := newHPSession(validatorBalances())

	// First opt in
	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-1",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      50_000_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})
	require.True(t, result.Ok, "opt-in should succeed: %s", result.Msg)

	// Opt-out is blocked (HP_UNSTAKE_ENABLED = false) — verify it's rejected
	result = session.OptOutHP(ledgerSystem.HPStakeParams{
		Id:            "opt-out-1",
		From:          "hive:validator1",
		To:            "hive:validator1",
		Amount:        50_000_000,
		BlockHeight:   1,
		ElectionEpoch: 5,
		HiveAccount:   "validator1",
	})
	require.False(t, result.Ok, "opt-out should be blocked until Phase 5")

	// pending_hp should still be 50K (opt-out was rejected)
	pendingBal := session.GetBalance("hive:validator1", 1, "pending_hp")
	require.Equal(t, int64(50_000_000), pendingBal, "pending_hp should remain since opt-out was blocked")
}

// ============================================================
// BUG J: Revert behavior with HP ops
// ============================================================

func TestOptInHPRevert(t *testing.T) {
	session := newHPSession(validatorBalances())

	// Opt in
	result := session.OptInHP(ledgerSystem.HPStakeParams{
		Id:          "opt-in-revert",
		From:        "hive:validator1",
		To:          "hive:validator1",
		Amount:      50_000_000,
		BlockHeight: 1,
		HiveAccount: "validator1",
	})
	require.True(t, result.Ok, "opt-in should succeed: %s", result.Msg)

	// Verify balance changed
	consensusBal := session.GetBalance("hive:validator1", 1, "hive_consensus")
	require.Equal(t, int64(10_000_000), consensusBal, "consensus should be reduced after opt-in")

	// Revert the session
	session.Revert()

	// After revert, create a new read to verify the balance is back to original
	// Revert clears the session caches, so next GetBalance re-reads from the DB snapshot
	consensusAfterRevert := session.GetBalance("hive:validator1", 1, "hive_consensus")
	require.Equal(t, int64(60_000_000), consensusAfterRevert, "consensus should be restored after Revert()")
}

func TestRegressionConsensusUnstakeAfterHPChanges(t *testing.T) {
	session := newHPSession(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    0,
			HIVE_CONSENSUS: 10_000,
		}},
	})

	result := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "cu-regression",
		From:          "hive:alice",
		To:            "hive:alice",
		Amount:        5_000,
		BlockHeight:   1,
		ElectionEpoch: 10,
	})
	require.True(t, result.Ok, "consensus unstake should still work after HP changes")
}
