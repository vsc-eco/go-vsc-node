package ledgerSystem_test

// review8 GV-H1 — a transient Mongo fault in GetBalance panics the node.
//
// GetLedgerRange returns (nil, err) on a Mongo failure. GetBalance discarded
// the error and ranged over the nil slice pointer, so a transient DB blip in
// the consensus spend-check path crashed the process. Silently treating the
// error as "no records" is equally unsafe — it would compute a balance from a
// partial read and fork this node from healthy peers. The fix is fail-stop:
// block-and-retry until the read succeeds.
//
// This test drives a DB that errors twice then recovers and asserts GetBalance
// returns the correct balance without panicking.

import (
	"errors"
	"testing"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/require"
)

type flakyLedgerDb struct {
	*test_utils.MockLedgerDb
	failsLeft int
}

func (f *flakyLedgerDb) GetLedgerRange(account string, start, end uint64, asset string, options ...ledgerDb.LedgerOptions) (*[]ledgerDb.LedgerRecord, error) {
	if f.failsLeft > 0 {
		f.failsLeft--
		return nil, errors.New("mongo: server selection error: context deadline exceeded")
	}
	return f.MockLedgerDb.GetLedgerRange(account, start, end, asset, options...)
}

func TestReview8_GVH1_GetBalanceFailStopsNotPanics(t *testing.T) {
	mock := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	flaky := &flakyLedgerDb{MockLedgerDb: mock, failsLeft: 2}

	state := &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     100,
		LedgerDb:        flaky,
		ActionDb:        &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)},
		BalanceDb:       &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)},
	}
	_ = mock.StoreLedger(
		ledgerDb.LedgerRecord{Id: "d", To: "hive:acct", Amount: 100000, Asset: "hbd", Type: "deposit", BlockHeight: 10},
		ledgerDb.LedgerRecord{Id: "u", To: "hive:acct", Amount: 40000, Asset: "hbd", Type: "unstake", BlockHeight: 20},
	)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("GetBalance panicked on a transient DB error instead of fail-stop retrying: %v", r)
		}
	}()

	bal := state.GetBalance("hive:acct", 100, "hbd")
	require.Equal(t, int64(140000), bal, "balance must be correct after the DB recovers")
	require.Equal(t, 0, flaky.failsLeft, "GetBalance must have retried past the transient failures")
}
