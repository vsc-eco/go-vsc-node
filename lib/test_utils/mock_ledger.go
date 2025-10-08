package test_utils

import (
	"slices"
	"strings"
	"vsc-node/modules/aggregate"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

type MockBalanceDb struct {
	aggregate.Plugin
	BalanceRecords map[string][]ledgerDb.BalanceRecord
}

func (m *MockBalanceDb) GetBalanceRecord(account string, blockHeight uint64) (*ledgerDb.BalanceRecord, error) {
	if len(m.BalanceRecords[account]) == 0 {
		return nil, nil
	}
	var latestRecord ledgerDb.BalanceRecord
	for _, record := range m.BalanceRecords[account] {
		if record.BlockHeight <= blockHeight {
			latestRecord = record
		}
	}

	return &latestRecord, nil
}

func (m *MockBalanceDb) UpdateBalanceRecord(record ledgerDb.BalanceRecord) error {
	if _, exists := m.BalanceRecords[record.Account]; !exists {
		m.BalanceRecords[record.Account] = make([]ledgerDb.BalanceRecord, 0)
	}

	if len(m.BalanceRecords[record.Account]) > 0 && m.BalanceRecords[record.Account][len(m.BalanceRecords[record.Account])-1].BlockHeight == record.BlockHeight {
		m.BalanceRecords[record.Account][len(m.BalanceRecords[record.Account])-1] = record
	} else {
		m.BalanceRecords[record.Account] = append(m.BalanceRecords[record.Account], record)
	}

	return nil
}

func (m *MockBalanceDb) GetAll(blockHeight uint64) []ledgerDb.BalanceRecord {
	out := make([]ledgerDb.BalanceRecord, 0)
	for _, records := range m.BalanceRecords {
		if len(records) > 0 {
			out = append(out, records[len(records)-1])
		}
	}
	return out
}

type MockLedgerDb struct {
	aggregate.Plugin
	LedgerRecords map[string][]ledgerDb.LedgerRecord
}

func (m *MockLedgerDb) StoreLedger(ledgerRecords ...ledgerDb.LedgerRecord) {
	for _, record := range ledgerRecords {
		m.LedgerRecords[record.Owner] = append(m.LedgerRecords[record.Owner], record)
	}
}

func (m *MockLedgerDb) GetLedgerAfterHeight(account string, blockHeight uint64, asset string, limit *int64) (*[]ledgerDb.LedgerRecord, error) {
	das := m.LedgerRecords[account]
	filteredResults := make([]ledgerDb.LedgerRecord, 0)
	for _, record := range das {
		if record.BlockHeight >= blockHeight && record.Asset == asset {
			filteredResults = append(filteredResults, record)
		}
		if limit != nil && len(filteredResults) == int(*limit) {
			break
		}
	}
	return &filteredResults, nil
}

func (m *MockLedgerDb) GetLedgerRange(account string, start uint64, end uint64, asset string, options ...ledgerDb.LedgerOptions) (*[]ledgerDb.LedgerRecord, error) {
	das := m.LedgerRecords[account]
	opTypes := make([]string, 0)
	filteredResults := make([]ledgerDb.LedgerRecord, 0)
	for _, options := range options {
		if len(options.OpType) > 0 {
			opTypes = append(opTypes, options.OpType...)
		}
	}
	for _, record := range das {
		if (asset == "" || record.Asset == asset) && (len(opTypes) == 0 || slices.Contains(opTypes, record.Type)) {
			if record.BlockHeight >= start && record.BlockHeight <= end {
				filteredResults = append(filteredResults, record)
			}
		}
	}

	return &filteredResults, nil
}

// GraphQL use only, not implemented in mocks
func (m *MockLedgerDb) GetLedgersTsRange(account *string, txId *string, txTypes []string, asset *ledgerDb.Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledgerDb.LedgerRecord, error) {
	return make([]ledgerDb.LedgerRecord, 0), nil
}

// GraphQL use only, not implemented in mocks
func (m *MockLedgerDb) GetRawLedgerRange(account *string, txId *string, txTypes []string, asset *ledgerDb.Asset, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledgerDb.LedgerRecord, error) {
	return make([]ledgerDb.LedgerRecord, 0), nil
}

func (m *MockLedgerDb) GetDistinctAccountsRange(startBlock, endBlock uint64) ([]string, error) {
	results := make([]string, 0)
	for acc, records := range m.LedgerRecords {
		exists := false
		for _, record := range records {
			if record.BlockHeight >= startBlock && record.BlockHeight <= endBlock {
				exists = true
				break
			}
		}
		if exists {
			results = append(results, acc)
		}
	}

	return results, nil
}

type MockInterestClaimsDb struct {
	aggregate.Plugin
	Claims []ledgerDb.ClaimRecord
}

func (ic *MockInterestClaimsDb) GetLastClaim(blockHeight uint64) *ledgerDb.ClaimRecord {
	var result ledgerDb.ClaimRecord
	for _, claim := range ic.Claims {
		if claim.BlockHeight < blockHeight {
			result = claim
		}
	}
	return &result
}

func (ic *MockInterestClaimsDb) SaveClaim(claim ledgerDb.ClaimRecord) {
	ic.Claims = append(ic.Claims, claim)
}

type MockActionsDb struct {
	aggregate.Plugin
	Actions map[string]ledgerDb.ActionRecord
}

func (m *MockActionsDb) StoreAction(action ledgerDb.ActionRecord) {
	m.Actions[action.Id] = action
}

func (m *MockActionsDb) ExecuteComplete(actionId *string, ids ...string) {
	for _, id := range ids {
		action, exists := m.Actions[id]
		if exists {
			action.Status = "complete"
			if actionId != nil {
				action.TxId = *actionId
			}
			m.Actions[id] = action
		}
	}
}

func (m *MockActionsDb) Get(id string) (*ledgerDb.ActionRecord, error) {
	d, exists := m.Actions[id]
	if !exists {
		return nil, nil
	}
	return &d, nil
}

func (m *MockActionsDb) SetStatus(id string, status string) {
	action := m.Actions[id]
	action.Status = status
	m.Actions[id] = action
}

// Multisig gatway use only, not implemented in mocks
func (m *MockActionsDb) GetPendingActions(bh uint64, t ...string) ([]ledgerDb.ActionRecord, error) {
	return make([]ledgerDb.ActionRecord, 0), nil
}

func (m *MockActionsDb) GetPendingActionsByEpoch(epoch uint64, t ...string) ([]ledgerDb.ActionRecord, error) {
	result := make([]ledgerDb.ActionRecord, 0)
	for _, action := range m.Actions {
		if slices.Contains(t, action.Type) && action.Params != nil && action.Params["epoch"] == epoch {
			result = append(result, action)
		}
	}
	return result, nil
}

// GraphQL use only, not implemented in mocks
func (m *MockActionsDb) GetActionsRange(txId *string, actionId *string, account *string, byTypes []string, asset *ledgerDb.Asset, status *string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledgerDb.ActionRecord, error) {
	return make([]ledgerDb.ActionRecord, 0), nil
}

func (m *MockActionsDb) GetAccountPendingConsensusUnstake(account string) (int64, error) {
	result := int64(0)
	for _, action := range m.Actions {
		if action.To == account && action.Type == "consensus_unstake" && action.Status == "pending" {
			result = result + action.Amount
		}
	}
	return result, nil
}

func (m *MockActionsDb) GetActionsByTxId(txId string) ([]ledgerDb.ActionRecord, error) {
	result := make([]ledgerDb.ActionRecord, 0)
	for id, action := range m.Actions {
		if strings.HasPrefix(id, txId) {
			result = append(result, action)
		}
	}
	return result, nil
}
