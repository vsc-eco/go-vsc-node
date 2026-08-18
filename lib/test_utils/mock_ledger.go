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
	GetAllErr      error
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

func (m *MockBalanceDb) DeleteBalanceRecordsFrom(account string, fromHeight uint64) error {
	kept := make([]ledgerDb.BalanceRecord, 0, len(m.BalanceRecords[account]))
	for _, r := range m.BalanceRecords[account] {
		if r.BlockHeight >= fromHeight {
			continue
		}
		kept = append(kept, r)
	}
	m.BalanceRecords[account] = kept
	return nil
}

func (m *MockBalanceDb) GetAll(blockHeight uint64) ([]ledgerDb.BalanceRecord, error) {
	if m.GetAllErr != nil {
		return nil, m.GetAllErr
	}
	out := make([]ledgerDb.BalanceRecord, 0)
	for _, records := range m.BalanceRecords {
		if len(records) > 0 {
			out = append(out, records[len(records)-1])
		}
	}
	return out, nil
}

type MockLedgerDb struct {
	aggregate.Plugin
	LedgerRecords map[string][]ledgerDb.LedgerRecord
}

// StoreLedger upserts by Id within the per-owner slice — matching the
// production Mongo semantics where every consensus-relevant ledger write is
// keyed on a deterministic Id and a duplicate Id replaces (not appends) the
// existing row. This lets idempotency-by-design code (cancel/reverse, slash
// detectors) be exercised by mock-backed tests without false-positive
// double-counts.
func (m *MockLedgerDb) StoreLedger(ledgerRecords ...ledgerDb.LedgerRecord) error {
	for _, record := range ledgerRecords {
		owner := record.Owner
		existing := m.LedgerRecords[owner]
		// Records with empty Id fall through to append (legacy flow).
		if record.Id != "" {
			replaced := false
			for i := range existing {
				if existing[i].Id == record.Id {
					existing[i] = record
					replaced = true
					break
				}
			}
			if replaced {
				m.LedgerRecords[owner] = existing
				continue
			}
		}
		m.LedgerRecords[owner] = append(existing, record)
	}
	return nil
}

// DeleteLegacyInterestRecords mirrors the production delete: drop interest rows
// at recordBlockHeight whose Id has no '#' (the old index-based scheme), leaving
// account-keyed rows intact.
func (m *MockLedgerDb) DeleteLegacyInterestRecords(recordBlockHeight uint64, owners []string) error {
	if len(owners) == 0 {
		return nil
	}
	scope := make(map[string]bool, len(owners))
	for _, o := range owners {
		scope[o] = true
	}
	for owner, records := range m.LedgerRecords {
		kept := make([]ledgerDb.LedgerRecord, 0, len(records))
		for _, r := range records {
			// Match the real query: scoped by the record's Owner field, not the
			// map key, so an account that gets no replacement row keeps its
			// legacy credit.
			if r.Type == "interest" && r.BlockHeight == recordBlockHeight &&
				!strings.Contains(r.Id, "#") && scope[r.Owner] {
				continue
			}
			kept = append(kept, r)
		}
		m.LedgerRecords[owner] = kept
	}
	return nil
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

func (ic *MockInterestClaimsDb) FindClaims(fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]ledgerDb.ClaimRecord, error) {
	return make([]ledgerDb.ClaimRecord, 0), nil
}

type MockActionsDb struct {
	aggregate.Plugin
	Actions map[string]ledgerDb.ActionRecord
	// GetErrs lets a test inject transient Get failures: each call pops one
	// error off the front. Exists so the IndexActions hardening (a DB fault
	// must never be mistaken for "action not found") can actually be
	// exercised — without it that path has no coverage at all.
	GetErrs []error
	// CompletedAt records, per action id, how many ledger records existed when
	// ExecuteComplete fired. It is how a test proves the credit was written
	// BEFORE the action was marked complete.
	CompletedAt map[string]int
	// LedgerRef, when set, is read to size CompletedAt.
	LedgerRef *MockLedgerDb
}

func (m *MockActionsDb) StoreAction(action ledgerDb.ActionRecord) {
	m.Actions[action.Id] = action
}

func (m *MockActionsDb) ExecuteComplete(actionId *string, ids ...string) {
	for _, id := range ids {
		if m.CompletedAt != nil {
			// Record only the FIRST completion for an id. A later call must not
			// be able to overwrite an earlier (premature) one, or an
			// ordering bug would mask itself.
			if _, seen := m.CompletedAt[id]; !seen {
				n := 0
				if m.LedgerRef != nil {
					for _, recs := range m.LedgerRef.LedgerRecords {
						n += len(recs)
					}
				}
				m.CompletedAt[id] = n
			}
		}
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

func (m *MockActionsDb) RevertProcessingToPending() ([]ledgerDb.ActionRecord, error) {
	reverted := make([]ledgerDb.ActionRecord, 0)
	for id, action := range m.Actions {
		if action.Status == "processing" {
			action.Status = "pending"
			m.Actions[id] = action
			reverted = append(reverted, action)
		}
	}
	return reverted, nil
}

func (m *MockActionsDb) Get(id string) (*ledgerDb.ActionRecord, error) {
	if len(m.GetErrs) > 0 {
		err := m.GetErrs[0]
		m.GetErrs = m.GetErrs[1:]
		if err != nil {
			return nil, err
		}
	}
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

// Faithful in-memory mirror of actionsDb.GetPendingActions:
// status=="pending", block_height<=bh, type in t (when t non-empty),
// sorted by block_height then id.
func (m *MockActionsDb) GetPendingActions(bh uint64, t ...string) ([]ledgerDb.ActionRecord, error) {
	result := make([]ledgerDb.ActionRecord, 0)
	for _, action := range m.Actions {
		if action.Status != "pending" {
			continue
		}
		if action.BlockHeight > bh {
			continue
		}
		if len(t) > 0 && !slices.Contains(t, action.Type) {
			continue
		}
		result = append(result, action)
	}
	slices.SortFunc(result, func(a, b ledgerDb.ActionRecord) int {
		if a.BlockHeight != b.BlockHeight {
			return int(a.BlockHeight) - int(b.BlockHeight)
		}
		return strings.Compare(a.Id, b.Id)
	})
	return result, nil
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
