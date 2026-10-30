package state_engine_test

import (
	"errors"
	"testing"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/require"
)

// flakyCancelLedgerDb fails its next `failsLeft` GetLedgerRange calls the way
// the real DB does on a Mongo failure: (nil, err).
type flakyCancelLedgerDb struct {
	*test_utils.MockLedgerDb
	failsLeft int
}

func (f *flakyCancelLedgerDb) GetLedgerRange(account string, start, end uint64, asset string, options ...ledgerDb.LedgerOptions) (*[]ledgerDb.LedgerRecord, error) {
	if f.failsLeft > 0 {
		f.failsLeft--
		return nil, errors.New("injected ledger read error")
	}
	return f.MockLedgerDb.GetLedgerRange(account, start, end, asset, options...)
}

// A transient read error while looking up the pending burn must not turn into
// "no pending burn rows": that node would skip the cancel records its peers
// write. The read is retried and the cancel lands.
func TestCancelPendingSafetySlashBurnRetriesPendingRead(t *testing.T) {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{Account: "hive:alice", BlockHeight: 100, HIVE_CONSENSUS: 1_000_000}},
	})
	lDb := &flakyCancelLedgerDb{MockLedgerDb: newMockLedgerDb()}
	ls := ledgerSystem.New(balDb, lDb, nil, newMockActionsDb(), nil)
	res := ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            "tx-cancel",
		BlockHeight:     200,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: 100,
	})
	require.True(t, res.Ok)
	require.Equal(t, int64(100_000), lDbPendingSlashBalance(t, lDb.MockLedgerDb))

	lDb.failsLeft = 1 // the first read of the cancel is the pending-row lookup
	cancelRes := ls.CancelPendingSafetySlashBurn(ledgerSystem.CancelPendingSafetySlashBurnParams{
		TxID:           "tx-cancel",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		BlockHeight:    250,
		Reason:         "rollback",
	})
	require.True(t, cancelRes.Ok, "cancel after a transient read error: %s", cancelRes.Msg)
	require.Equal(t, 0, lDb.failsLeft, "the pending read must have hit the injected error")
	require.Equal(t, int64(0), lDbPendingSlashBalance(t, lDb.MockLedgerDb), "pending must net to 0 after cancel")
}
