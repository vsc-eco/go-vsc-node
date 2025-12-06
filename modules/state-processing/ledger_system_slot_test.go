package state_engine_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

func TestLedgerSessionPreventsSameBlockDoubleSpend(t *testing.T) {
	ledgerDbMock := newMockLedgerDb()
	balanceDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    0,
			HBD:            10_000,
			HBD_SAVINGS:    10_000,
			Hive:           10_000,
			HIVE_CONSENSUS: 10_000,
		}},
	})
	actionsDb := newMockActionsDb()

	newSession := func() ledgerSystem.LedgerSession {
		state := &ledgerSystem.LedgerState{
			Oplog:           make([]ledgerSystem.OpLogEvent, 0),
			VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
			GatewayBalances: make(map[string]uint64),
			BlockHeight:     1,
			LedgerDb:        ledgerDbMock,
			BalanceDb:       balanceDb,
			ActionDb:        actionsDb,
		}
		return ledgerSystem.NewSession(state)
	}

	assertInsufficient := func(name string, result ledgerSystem.LedgerResult) {
		t.Logf("%s failed: %+v", name, result)
		require.False(t, result.Ok)
		require.Equal(t, "insufficient balance", result.Msg)
	}

	t.Run("stake HBD", func(t *testing.T) {
		session := newSession()
		require.True(t, session.Stake(ledgerSystem.StakeOp{
			OpLogEvent: ledgerSystem.OpLogEvent{
				Id:          "stake-hbd-1",
				From:        "hive:alice",
				To:          "system:staking",
				Asset:       "hbd",
				Amount:      7_000,
				BlockHeight: 1,
			},
		}).Ok)

		assertInsufficient("stake HBD second attempt", session.Stake(ledgerSystem.StakeOp{
			OpLogEvent: ledgerSystem.OpLogEvent{
				Id:          "stake-hbd-2",
				From:        "hive:alice",
				To:          "system:staking",
				Asset:       "hbd",
				Amount:      4_000,
				BlockHeight: 1,
			},
		}))
	})

	t.Run("unstake HBD savings", func(t *testing.T) {
		session := newSession()
		require.True(t, session.Unstake(ledgerSystem.StakeOp{
			OpLogEvent: ledgerSystem.OpLogEvent{
				Id:          "unstake-hbd-1",
				From:        "hive:alice",
				To:          "hive:alice",
				Asset:       "hbd_savings",
				Amount:      7_000,
				BlockHeight: 1,
			},
		}).Ok)

		assertInsufficient("unstake HBD second attempt", session.Unstake(ledgerSystem.StakeOp{
			OpLogEvent: ledgerSystem.OpLogEvent{
				Id:          "unstake-hbd-2",
				From:        "hive:alice",
				To:          "hive:alice",
				Asset:       "hbd_savings",
				Amount:      4_000,
				BlockHeight: 1,
			},
		}))
	})

	t.Run("consensus stake", func(t *testing.T) {
		session := newSession()
		require.True(t, session.ConsensusStake(ledgerSystem.ConsensusParams{
			Id:          "cs-1",
			From:        "hive:alice",
			To:          "hive:alice",
			Amount:      7_000,
			BlockHeight: 1,
			Type:        "stake",
		}).Ok)

		assertInsufficient("consensus stake second attempt", session.ConsensusStake(ledgerSystem.ConsensusParams{
			Id:          "cs-2",
			From:        "hive:alice",
			To:          "hive:alice",
			Amount:      4_000,
			BlockHeight: 1,
			Type:        "stake",
		}))
	})

	t.Run("consensus unstake", func(t *testing.T) {
		session := newSession()
		require.True(t, session.ConsensusUnstake(ledgerSystem.ConsensusParams{
			Id:            "cu-1",
			From:          "hive:alice",
			To:            "hive:alice",
			Amount:        7_000,
			BlockHeight:   1,
			Type:          "unstake",
			ElectionEpoch: 10,
		}).Ok)

		assertInsufficient("consensus unstake second attempt", session.ConsensusUnstake(ledgerSystem.ConsensusParams{
			Id:            "cu-2",
			From:          "hive:alice",
			To:            "hive:alice",
			Amount:        4_000,
			BlockHeight:   1,
			Type:          "unstake",
			ElectionEpoch: 10,
		}))
	})

	t.Run("transfer hbd", func(t *testing.T) {
		session := newSession()
		require.True(t, session.ExecuteTransfer(ledgerSystem.OpLogEvent{
			Id:          "transfer-hbd-1",
			From:        "hive:alice",
			To:          "hive:bob",
			Asset:       "hbd",
			Amount:      7_000,
			BlockHeight: 1,
		}).Ok)

		assertInsufficient("transfer HBD second attempt", session.ExecuteTransfer(ledgerSystem.OpLogEvent{
			Id:          "transfer-hbd-2",
			From:        "hive:alice",
			To:          "hive:bob",
			Asset:       "hbd",
			Amount:      4_000,
			BlockHeight: 1,
		}))
	})

	t.Run("transfer hive", func(t *testing.T) {
		session := newSession()
		require.True(t, session.ExecuteTransfer(ledgerSystem.OpLogEvent{
			Id:          "transfer-hive-1",
			From:        "hive:alice",
			To:          "hive:bob",
			Asset:       "hive",
			Amount:      7_000,
			BlockHeight: 1,
		}).Ok)

		assertInsufficient("transfer hive second attempt", session.ExecuteTransfer(ledgerSystem.OpLogEvent{
			Id:          "transfer-hive-2",
			From:        "hive:alice",
			To:          "hive:bob",
			Asset:       "hive",
			Amount:      4_000,
			BlockHeight: 1,
		}))
	})
}

func newMockBalanceDb(initial map[string][]ledgerDb.BalanceRecord) *test_utils.MockBalanceDb {
	cp := make(map[string][]ledgerDb.BalanceRecord)
	for k, records := range initial {
		cp[k] = append([]ledgerDb.BalanceRecord(nil), records...)
	}
	return &test_utils.MockBalanceDb{BalanceRecords: cp}
}

func newMockLedgerDb() *test_utils.MockLedgerDb {
	return &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
}

func newMockActionsDb() *test_utils.MockActionsDb {
	return &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
}
