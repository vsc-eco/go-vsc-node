package test_utils

import (
	"vsc-node/modules/aggregate"
	rcDb "vsc-node/modules/db/vsc/rcs"
)

type MockRcDb struct {
	aggregate.Plugin
	Records map[string][]rcDb.RcRecord
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

func (m *MockRcDb) SetRecord(account string, blockHeight uint64, amount int64) {
	m.Records[account] = append(m.Records[account], rcDb.RcRecord{
		Account:     account,
		Amount:      amount,
		BlockHeight: blockHeight,
	})
}
