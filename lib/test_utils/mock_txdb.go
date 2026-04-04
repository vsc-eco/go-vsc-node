package test_utils

import (
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db/vsc/transactions"
)

type MockTxDb struct {
	aggregate.Plugin
	Records map[string]transactions.TransactionRecord
}

func (m *MockTxDb) Ingest(offTx transactions.IngestTransactionUpdate) error {
	m.Records[offTx.Id] = transactions.TransactionRecord{
		Id:                   offTx.Id,
		Status:               transactions.TransactionStatus(offTx.Status),
		RequiredAuths:        offTx.RequiredAuths,
		RequiredPostingAuths: offTx.RequiredPostingAuths,
		Type:                 offTx.Type,
		Version:              offTx.Version,
		Nonce:                int64(offTx.Nonce),
		Ops:                  offTx.Ops,
		OpTypes:              offTx.OpTypes,
		RcLimit:              offTx.RcLimit,
		Ledger:               &offTx.Ledger,
	}
	return nil
}

func (m *MockTxDb) SetOutput(sOut transactions.SetResultUpdate) {
	rec, exists := m.Records[sOut.Id]
	if !exists {
		return
	}
	if sOut.Status != nil {
		rec.Status = *sOut.Status
	}
	if sOut.Ledger != nil {
		rec.Ledger = sOut.Ledger
	}
	if sOut.Output != nil {
		rec.Output = append(rec.Output, *sOut.Output)
	}
	m.Records[sOut.Id] = rec
}

func (m *MockTxDb) GetTransaction(id string) *transactions.TransactionRecord {
	rec, exists := m.Records[id]
	if !exists {
		return nil
	}
	return &rec
}

func (m *MockTxDb) FindTransactions(ids []string, id *string, account *string, contract *string, status *transactions.TransactionStatus, byType []string, ledgerToFrom *string, ledgerTypes []string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]transactions.TransactionRecord, error) {
	return make([]transactions.TransactionRecord, 0), nil
}

func (m *MockTxDb) FindUnconfirmedTransactions(height uint64) ([]transactions.TransactionRecord, error) {
	var results []transactions.TransactionRecord
	for _, rec := range m.Records {
		if rec.Status == transactions.TransactionStatusUnconfirmed {
			results = append(results, rec)
		}
	}
	return results, nil
}
