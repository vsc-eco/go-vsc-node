package state_engine_test

import (
	"slices"
	"testing"

	"github.com/chebyrash/promise"
	"github.com/stretchr/testify/require"

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

type mockBalanceDb struct {
	records map[string][]ledgerDb.BalanceRecord
}

func newMockBalanceDb(initial map[string][]ledgerDb.BalanceRecord) *mockBalanceDb {
	cp := make(map[string][]ledgerDb.BalanceRecord)
	for k, records := range initial {
		cp[k] = append([]ledgerDb.BalanceRecord(nil), records...)
	}
	return &mockBalanceDb{records: cp}
}

func (m *mockBalanceDb) Init() error {
	return nil
}

func (m *mockBalanceDb) Start() *promise.Promise[any] {
	return resolvedPromise()
}

func (m *mockBalanceDb) Stop() error {
	return nil
}

func (m *mockBalanceDb) GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error) {
	records := m.records[account]
	var result *ledgerDb.BalanceRecord
	for i := range records {
		rec := records[i]
		if rec.BlockHeight <= blockHeight {
			tmp := rec
			result = &tmp
		}
	}
	return result, nil
}

func (m *mockBalanceDb) UpdateBalanceRecord(record ledgerDb.BalanceRecord) error {
	m.records[record.Account] = append(m.records[record.Account], record)
	return nil
}

func (m *mockBalanceDb) GetAll(blockHeight uint64) []ledgerDb.BalanceRecord {
	out := make([]ledgerDb.BalanceRecord, 0, len(m.records))
	for _, records := range m.records {
		if len(records) == 0 {
			continue
		}
		out = append(out, records[len(records)-1])
	}
	return out
}

type mockLedgerDb struct {
	records map[string][]ledgerDb.LedgerRecord
}

func newMockLedgerDb() *mockLedgerDb {
	return &mockLedgerDb{records: make(map[string][]ledgerDb.LedgerRecord)}
}

func (m *mockLedgerDb) Init() error {
	return nil
}

func (m *mockLedgerDb) Start() *promise.Promise[any] {
	return resolvedPromise()
}

func (m *mockLedgerDb) Stop() error {
	return nil
}

func (m *mockLedgerDb) StoreLedger(records ...ledgerDb.LedgerRecord) {
	for _, record := range records {
		m.records[record.Owner] = append(m.records[record.Owner], record)
	}
}

func (m *mockLedgerDb) GetLedgerAfterHeight(account string, blockHeight uint64, asset string, limit *int64) (*[]ledgerDb.LedgerRecord, error) {
	out := make([]ledgerDb.LedgerRecord, 0)
	return &out, nil
}

func (m *mockLedgerDb) GetLedgerRange(account string, start uint64, end uint64, asset string, options ...ledgerDb.LedgerOptions) (*[]ledgerDb.LedgerRecord, error) {
	var opTypes []string
	for _, opt := range options {
		opTypes = append(opTypes, opt.OpType...)
	}
	out := make([]ledgerDb.LedgerRecord, 0)
	for _, record := range m.records[account] {
		if asset != "" && record.Asset != asset {
			continue
		}
		if record.BlockHeight < start || record.BlockHeight > end {
			continue
		}
		if len(opTypes) > 0 && !slices.Contains(opTypes, record.Type) {
			continue
		}
		out = append(out, record)
	}
	return &out, nil
}

func (m *mockLedgerDb) GetLedgersTsRange(account *string, txId *string, txTypes []string, asset *ledgerDb.Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledgerDb.LedgerRecord, error) {
	return []ledgerDb.LedgerRecord{}, nil
}

func (m *mockLedgerDb) GetRawLedgerRange(account *string, txId *string, txTypes []string, asset *ledgerDb.Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledgerDb.LedgerRecord, error) {
	return []ledgerDb.LedgerRecord{}, nil
}

func (m *mockLedgerDb) GetDistinctAccountsRange(startBlock, endBlock uint64) ([]string, error) {
	accounts := make([]string, 0, len(m.records))
	for acc := range m.records {
		accounts = append(accounts, acc)
	}
	return accounts, nil
}

type mockActionsDb struct {
	actions map[string]ledgerDb.ActionRecord
}

func newMockActionsDb() *mockActionsDb {
	return &mockActionsDb{actions: make(map[string]ledgerDb.ActionRecord)}
}

func (m *mockActionsDb) Init() error {
	return nil
}

func (m *mockActionsDb) Start() *promise.Promise[any] {
	return resolvedPromise()
}

func (m *mockActionsDb) Stop() error {
	return nil
}

func (m *mockActionsDb) StoreAction(action ledgerDb.ActionRecord) {
	m.actions[action.Id] = action
}

func (m *mockActionsDb) ExecuteComplete(actionId *string, ids ...string) {
	for _, id := range ids {
		if action, ok := m.actions[id]; ok {
			action.Status = "complete"
			if actionId != nil {
				action.TxId = *actionId
			}
			m.actions[id] = action
		}
	}
}

func (m *mockActionsDb) Get(id string) (*ledgerDb.ActionRecord, error) {
	if action, ok := m.actions[id]; ok {
		return &action, nil
	}
	return nil, nil
}

func (m *mockActionsDb) SetStatus(id string, status string) {
	if action, ok := m.actions[id]; ok {
		action.Status = status
		m.actions[id] = action
	}
}

func (m *mockActionsDb) GetPendingActions(bh uint64, t ...string) ([]ledgerDb.ActionRecord, error) {
	return []ledgerDb.ActionRecord{}, nil
}

func (m *mockActionsDb) GetPendingActionsByEpoch(epoch uint64, t ...string) ([]ledgerDb.ActionRecord, error) {
	return []ledgerDb.ActionRecord{}, nil
}

func (m *mockActionsDb) GetActionsRange(txId *string, actionId *string, account *string, byTypes []string, asset *ledgerDb.Asset, status *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledgerDb.ActionRecord, error) {
	return []ledgerDb.ActionRecord{}, nil
}

func (m *mockActionsDb) GetAccountPendingConsensusUnstake(account string) (int64, error) {
	total := int64(0)
	for _, action := range m.actions {
		if action.To == account && action.Type == "consensus_unstake" && action.Status != "complete" {
			total += action.Amount
		}
	}
	return total, nil
}

func (m *mockActionsDb) GetActionsByTxId(txId string) ([]ledgerDb.ActionRecord, error) {
	out := make([]ledgerDb.ActionRecord, 0)
	for _, action := range m.actions {
		if action.TxId == txId || action.Id == txId {
			out = append(out, action)
		}
	}
	return out, nil
}

func resolvedPromise() *promise.Promise[any] {
	return promise.New(func(resolve func(any), reject func(error)) {
		resolve(nil)
	})
}
