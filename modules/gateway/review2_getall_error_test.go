package gateway

import (
	"errors"
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

func TestSyncBalanceReturnsErrorOnGetAllFailure(t *testing.T) {
	const bh = uint64(7200) // ACTION_INTERVAL aligned

	balDb := &test_utils.MockBalanceDb{
		BalanceRecords: make(map[string][]ledgerDb.BalanceRecord),
		GetAllErr:      errors.New("mongodb connection refused"),
	}

	ms := &MultiSig{
		sconf:     systemconfig.MocknetConfig(),
		balanceDb: balDb,
	}

	_, err := ms.syncBalance(bh)
	if err == nil {
		t.Fatal("syncBalance should return error when GetAll fails")
	}
	if err.Error() != "balance enumeration failed" {
		t.Fatalf("expected 'balance enumeration failed', got %q", err.Error())
	}
}

