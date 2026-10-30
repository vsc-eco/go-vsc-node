package test_utils

import (
	"vsc-node/modules/aggregate"
	rcDb "vsc-node/modules/db/vsc/rcs"
)

type MockRcDb struct {
	aggregate.Plugin
	Records map[string][]rcDb.RcRecord
	// SetRecordErr lets a test inject a persistence failure so the caller's
	// handling of it can be exercised.
	SetRecordErr error
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

func (m *MockRcDb) SetRecord(account string, blockHeight uint64, amount int64) error {
	if m.SetRecordErr != nil {
		return m.SetRecordErr
	}
	m.Records[account] = append(m.Records[account], rcDb.RcRecord{
		Account:     account,
		Amount:      amount,
		BlockHeight: blockHeight,
	})
	return nil
}

// SetRecords mirrors SetRecord for the batched path.
func (m *MockRcDb) SetRecords(records []rcDb.RcRecord) error {
	for _, r := range records {
		m.SetRecord(r.Account, r.BlockHeight, r.Amount)
	}
	return nil
}
