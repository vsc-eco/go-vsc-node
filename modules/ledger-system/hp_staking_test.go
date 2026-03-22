package ledgerSystem

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ============================================================
// EXECUTEOPLOG TESTS for hp_stake / hp_unstake
// These are in the ledgerSystem package to access unexported fields
// ============================================================

func TestExecuteOplogHPStake(t *testing.T) {
	oplog := []OpLogEvent{
		{
			Id:          "hp-stake-1",
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      50_000_000,
			Asset:       "hive",
			Type:        "hp_stake",
			BlockHeight: 100,
			Params: map[string]interface{}{
				"hive_account": "validator1",
			},
		},
	}

	result := ExecuteOplog(oplog, 0, 100)

	require.GreaterOrEqual(t, len(result.ledgerRecords), 2,
		"hp_stake should produce at least 2 ledger records")

	var debit, credit *LedgerUpdate
	for i := range result.ledgerRecords {
		r := &result.ledgerRecords[i]
		if r.Amount < 0 {
			debit = r
		} else if r.Amount > 0 {
			credit = r
		}
	}

	require.NotNil(t, debit, "should have a debit record")
	require.NotNil(t, credit, "should have a credit record")

	require.Equal(t, "hive_consensus", debit.Asset, "debit should be from hive_consensus")
	require.Equal(t, int64(-50_000_000), debit.Amount)
	require.Equal(t, "hive:validator1", debit.Owner)

	require.Equal(t, "pending_hp", credit.Asset, "credit should be to pending_hp (two-phase commit)")
	require.Equal(t, int64(50_000_000), credit.Amount)
	require.Equal(t, "hive:validator1", credit.Owner)

	require.GreaterOrEqual(t, len(result.actionRecords), 1,
		"hp_stake should create an action record for gateway processing")
	require.Equal(t, "hp_stake", result.actionRecords[0].Type)
	require.Equal(t, "pending", result.actionRecords[0].Status)

	// BUG I: Verify action record Params contains expected keys
	require.Contains(t, result.actionRecords[0].Params, "hive_account",
		"hp_stake action record should contain 'hive_account' param")
}

func TestExecuteOplogHPUnstake_Blocked(t *testing.T) {
	// HP_UNSTAKE_ENABLED is false (Phase 5 pending), so ExecuteOplog
	// must skip hp_unstake events entirely — defense-in-depth guard.
	require.False(t, HP_UNSTAKE_ENABLED, "HP_UNSTAKE_ENABLED should be false until Phase 5")

	oplog := []OpLogEvent{
		{
			Id:          "hp-unstake-1",
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      50_000_000,
			Asset:       "hive",
			Type:        "hp_unstake",
			BlockHeight: 200,
			Params: map[string]interface{}{
				"epoch":        uint64(15),
				"hive_account": "validator1",
			},
		},
	}

	result := ExecuteOplog(oplog, 0, 200)

	require.Equal(t, 0, len(result.ledgerRecords),
		"hp_unstake should produce NO ledger records when HP_UNSTAKE_ENABLED=false")
	require.Equal(t, 0, len(result.actionRecords),
		"hp_unstake should produce NO action records when HP_UNSTAKE_ENABLED=false")
}

// ============================================================
// EXECUTEOPLOG TEST for hp_confirm (pending_hp -> hive_hp)
// ============================================================

func TestExecuteOplogHPConfirm(t *testing.T) {
	oplog := []OpLogEvent{
		{
			Id:          "hp-confirm-1",
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      50_000_000,
			Asset:       "hive",
			Type:        "hp_confirm",
			BlockHeight: 300,
			Params: map[string]interface{}{
				"hive_account": "validator1",
			},
		},
	}

	result := ExecuteOplog(oplog, 0, 300)

	require.Equal(t, 2, len(result.ledgerRecords),
		"hp_confirm should produce exactly 2 ledger records")

	debit := result.ledgerRecords[0]
	credit := result.ledgerRecords[1]

	require.Equal(t, int64(-50_000_000), debit.Amount)
	require.Equal(t, "pending_hp", debit.Asset, "debit should be from pending_hp")
	require.Equal(t, "hive:validator1", debit.Owner)
	require.Equal(t, "hp_confirm", debit.Type)

	require.Equal(t, int64(50_000_000), credit.Amount)
	require.Equal(t, "hive_hp", credit.Asset, "credit should be to hive_hp")
	require.Equal(t, "hive:validator1", credit.Owner)
	require.Equal(t, "hp_confirm", credit.Type)

	// hp_confirm should NOT create action records (it's the completion of the two-phase)
	require.Equal(t, 0, len(result.actionRecords),
		"hp_confirm should produce no action records")
}

// ============================================================
// REGRESSION — existing ExecuteOplog types unchanged
// ============================================================

func TestRegressionExecuteOplogConsensusStake(t *testing.T) {
	oplog := []OpLogEvent{
		{
			Id:     "cs-1",
			From:   "hive:alice",
			To:     "hive:alice",
			Amount: 5_000,
			Asset:  "hive",
			Type:   "consensus_stake",
		},
	}
	result := ExecuteOplog(oplog, 0, 100)

	require.Equal(t, 2, len(result.ledgerRecords), "consensus_stake should produce 2 records")
	require.Equal(t, int64(-5_000), result.ledgerRecords[0].Amount)
	require.Equal(t, "hive", result.ledgerRecords[0].Asset)
	require.Equal(t, int64(5_000), result.ledgerRecords[1].Amount)
	require.Equal(t, "hive_consensus", result.ledgerRecords[1].Asset)
}

func TestRegressionExecuteOplogTransfer(t *testing.T) {
	oplog := []OpLogEvent{
		{
			Id:     "t-1",
			From:   "hive:alice",
			To:     "hive:bob",
			Amount: 1_000,
			Asset:  "hbd",
			Type:   "transfer",
		},
	}
	result := ExecuteOplog(oplog, 0, 100)

	require.Equal(t, 2, len(result.ledgerRecords), "transfer should produce 2 records")
	require.Equal(t, int64(-1_000), result.ledgerRecords[0].Amount)
	require.Equal(t, int64(1_000), result.ledgerRecords[1].Amount)
}

func TestRegressionExecuteOplogConsensusUnstake(t *testing.T) {
	oplog := []OpLogEvent{
		{
			Id:     "cu-1",
			From:   "hive:alice",
			To:     "hive:alice",
			Amount: 5_000,
			Asset:  "hive",
			Type:   "consensus_unstake",
			Params: map[string]interface{}{
				"epoch": uint64(10),
			},
		},
	}
	result := ExecuteOplog(oplog, 0, 100)

	require.Equal(t, 1, len(result.ledgerRecords), "consensus_unstake should produce 1 record")
	require.Equal(t, int64(-5_000), result.ledgerRecords[0].Amount)
	require.Equal(t, "hive_consensus", result.ledgerRecords[0].Asset)
	require.Equal(t, 1, len(result.actionRecords))
	require.Equal(t, "consensus_unstake", result.actionRecords[0].Type)
}
