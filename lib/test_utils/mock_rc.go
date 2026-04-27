package test_utils

import (
	"context"
	"vsc-node/modules/aggregate"
	rcDb "vsc-node/modules/db/vsc/rcs"
)

type MockRcDb struct {
	aggregate.Plugin
	Records map[string][]rcDb.RcRecord
}

func NewMockRcDb() *MockRcDb {
	return &MockRcDb{Records: make(map[string][]rcDb.RcRecord)}
}

func (m *MockRcDb) GetRecord(account string, blockHeight uint64) (rcDb.RcRecord, error) {
	recs := m.Records[account]
	var best rcDb.RcRecord
	for _, r := range recs {
		if r.BlockHeight <= blockHeight && r.BlockHeight >= best.BlockHeight {
			best = r
		}
	}
	return best, nil
}

func (m *MockRcDb) GetRecordsBulk(ctx context.Context, accounts []string, blockHeight uint64) (map[string]rcDb.RcRecord, error) {
	out := make(map[string]rcDb.RcRecord, len(accounts))
	for _, account := range accounts {
		rec, _ := m.GetRecord(account, blockHeight)
		if rec.BlockHeight != 0 {
			out[account] = rec
		}
	}
	return out, nil
}

func (m *MockRcDb) SetRecord(account string, blockHeight uint64, amount int64) {
	m.Records[account] = append(m.Records[account], rcDb.RcRecord{
		Account:     account,
		Amount:      amount,
		BlockHeight: blockHeight,
	})
}

func (m *MockRcDb) SetRecordsBulk(ctx context.Context, records []rcDb.RcRecord) error {
	for _, rec := range records {
		m.SetRecord(rec.Account, rec.BlockHeight, rec.Amount)
	}
	return nil
}

func (m *MockRcDb) PruneOlderThan(ctx context.Context, cutoff uint64) (int64, error) {
	deleted := int64(0)
	for account, recs := range m.Records {
		kept := make([]rcDb.RcRecord, 0, len(recs))
		for _, r := range recs {
			if r.BlockHeight < cutoff {
				deleted++
				continue
			}
			kept = append(kept, r)
		}
		m.Records[account] = kept
	}
	return deleted, nil
}
