package state_engine_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	state_engine "vsc-node/modules/state-processing"

	"vsc-node/modules/common/params"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	dbTransactions "vsc-node/modules/db/vsc/transactions"
	safetyslash "vsc-node/modules/incentive-pendulum/safety_slash"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// chainOpFixture wires a StateEngine with a real LedgerSystem + LedgerState
// + a test-only txDb so the chain-op apply functions exercise the same
// row-lookup and ledger-write paths they do in production.
type chainOpFixture struct {
	se *state_engine.StateEngine
	ls ledgerSystem.LedgerSystem
	db *test_utils.MockLedgerDb
	tx *fakeChainOpTxDb
}

// fakeChainOpTxDb implements dbTransactions.Transactions with the
// minimum surface the chain-op apply functions touch — currently
// GetTransaction only. The embedded aggregate.Plugin (left nil) covers
// the Init/Start/Stop methods on the interface; the chain-op apply path
// never calls them, so a nil embedded value is safe here.
type fakeChainOpTxDb struct {
	aggregate.Plugin
	records map[string]*dbTransactions.TransactionRecord
}

func (f *fakeChainOpTxDb) Ingest(_ dbTransactions.IngestTransactionUpdate) error { return nil }
func (f *fakeChainOpTxDb) SetOutput(_ dbTransactions.SetResultUpdate)            {}
func (f *fakeChainOpTxDb) GetTransaction(id string) *dbTransactions.TransactionRecord {
	return f.records[id]
}
func (f *fakeChainOpTxDb) FindTransactions(ids []string, id *string, account *string, contract *string, status *dbTransactions.TransactionStatus, byType []string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]dbTransactions.TransactionRecord, error) {
	return nil, nil
}
func (f *fakeChainOpTxDb) FindUnconfirmedTransactions(height uint64) ([]dbTransactions.TransactionRecord, error) {
	return nil, nil
}
func (f *fakeChainOpTxDb) InvalidateCompetingTransactions(_ []string, _ []uint64) (int64, error) {
	return 0, nil
}
func (f *fakeChainOpTxDb) HasUnconfirmedWithNonce(_ []string, _ uint64) (bool, error) {
	return false, nil
}

func newChainOpFixture(t *testing.T) *chainOpFixture {
	t.Helper()
	balDb := &test_utils.MockBalanceDb{
		BalanceRecords: map[string][]ledgerDb.BalanceRecord{
			"hive:alice": {{
				Account:        "hive:alice",
				BlockHeight:    50,
				HIVE_CONSENSUS: 1_000_000,
			}},
		},
	}
	lDb := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	aDb := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}

	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)
	state := ls.NewEmptyState()
	tx := &fakeChainOpTxDb{records: make(map[string]*dbTransactions.TransactionRecord)}

	se := state_engine.NewChainOpStateEngineForTest(ls, state, tx)
	return &chainOpFixture{se: se, ls: ls, db: lDb, tx: tx}
}

func (f *chainOpFixture) fireSlash(t *testing.T, txID string, blockHeight uint64, burnDelay uint64) {
	t.Helper()
	res := f.ls.SafetySlashConsensusBond(ledgerSystem.SafetySlashConsensusParams{
		Account:         "alice",
		SlashBps:        1000,
		TxID:            txID,
		BlockHeight:     blockHeight,
		EvidenceKind:    safetyslash.EvidenceVSCDoubleBlockSign,
		BurnDelayBlocks: burnDelay,
	})
	require.True(t, res.Ok, "fixture slash must succeed: %s", res.Msg)
}

// --- vsc.safety_slash_reverse handler tests ---------------------------------

func TestApplySafetySlashReverse_Cancel_RemovesPendingBurn(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-cancel", 100, 100)

	rec := safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-cancel",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionCancel,
		Reason:         "test cancel",
	}
	f.se.ApplySafetySlashReverseForTest(rec, "ctx", 150)

	pendingRows := f.db.LedgerRecords[params.ProtocolSlashPendingBurnAccount]
	var sawCancelMarker, sawRelease bool
	for _, r := range pendingRows {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingCancelled {
			sawCancelMarker = true
		}
		if r.Type == ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingRelease {
			sawRelease = true
		}
	}
	require.True(t, sawCancelMarker, "expected cancellation marker row")
	require.True(t, sawRelease, "expected cancel-release row")
}

func TestApplySafetySlashReverse_Reverse_CapsAtSlashAmount(t *testing.T) {
	// Destination-change semantics: a legitimate reverse uses the pending
	// challenge window (burnDelay>0) + ReverseActionBoth (cancel removes the
	// pending residual from the reserve-path so the bond can be restored). An
	// over-amount request must cap at the original slash amount.
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-rev", 100, 100)

	rec := safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-rev",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionBoth,
		Amount:         250_000,
	}
	f.se.ApplySafetySlashReverseForTest(rec, "ctx", 110)

	totalReverse := int64(0)
	for _, r := range f.db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			totalReverse += r.Amount
		}
	}
	require.Equal(t, int64(100_000), totalReverse,
		"reverse must cap at original slash amount")
}

func TestApplySafetySlashReverse_DoubleReverse_RespectsRunningTotal(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-2x", 100, 100)

	f.se.ApplySafetySlashReverseForTest(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-2x",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionBoth,
		Amount:         60_000,
	}, "ctx-1", 110)

	f.se.ApplySafetySlashReverseForTest(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-2x",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionBoth,
		Amount:         80_000,
		Reason:         "second-pass",
	}, "ctx-2", 120)

	totalReverse := int64(0)
	for _, r := range f.db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			totalReverse += r.Amount
		}
	}
	require.Equal(t, int64(100_000), totalReverse,
		"running-total cap must hold across multiple reverse ops")
}

// TestApplySafetySlashReverse_ReverseOnlyDuringWindow_Blocked locks in audit
// R2-F1: a reverse-only action during the challenge window must NOT re-credit
// the bond, because the pending residual is still in-flight to the reserve
// (uncancelledPendingResidualAmt counts it as committed). Otherwise the bond
// would be restored AND the residual would still mature into the reserve — a
// mint. To undo during the window, governance must use ReverseActionBoth.
func TestApplySafetySlashReverse_ReverseOnlyDuringWindow_Blocked(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-window", 100, 100) // residual sits pending, not yet reserve

	f.se.ApplySafetySlashReverseForTest(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-window",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionReverse, // reverse-only: does NOT cancel pending
		Amount:         100_000,
	}, "ctx", 110)

	totalReverse := int64(0)
	for _, r := range f.db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			totalReverse += r.Amount
		}
	}
	require.Equal(t, int64(0), totalReverse,
		"reverse-only during the window must be blocked (pending residual is committed-in-flight)")
}

// TestApplySafetySlashReverse_ImmediateReserve_NotReversible locks in that a
// burnDelay==0 slash (residual committed directly to the reserve, no challenge
// window) is NOT reversible — even via Both — because the residual is already a
// retained backstop in the reserve and re-crediting it would mint.
func TestApplySafetySlashReverse_ImmediateReserve_NotReversible(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-immediate", 100, 0) // residual lands directly in the reserve

	f.se.ApplySafetySlashReverseForTest(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-immediate",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionBoth,
		Amount:         100_000,
	}, "ctx", 110)

	totalReverse := int64(0)
	for _, r := range f.db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			totalReverse += r.Amount
		}
	}
	require.Equal(t, int64(0), totalReverse,
		"immediate-reserve slash (burnDelay=0) must not be reversible")
}

func TestApplySafetySlashReverse_Both_CancelsAndCredits(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-both", 100, 100)

	f.se.ApplySafetySlashReverseForTest(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-both",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionBoth,
		Amount:         100_000,
	}, "ctx", 150)

	pendingRows := f.db.LedgerRecords[params.ProtocolSlashPendingBurnAccount]
	sawCancelMarker := false
	for _, r := range pendingRows {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashHiveBurnPendingCancelled {
			sawCancelMarker = true
		}
	}
	require.True(t, sawCancelMarker, "both action must produce cancellation marker")

	totalReverse := int64(0)
	for _, r := range f.db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			totalReverse += r.Amount
		}
	}
	require.Equal(t, int64(100_000), totalReverse, "both action must credit hive_consensus")
}

func TestApplySafetySlashReverse_DropsOnMissingSlashRow(t *testing.T) {
	f := newChainOpFixture(t)

	rec := safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "no-such-slash",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionReverse,
		Amount:         10_000,
	}
	f.se.ApplySafetySlashReverseForTest(rec, "ctx", 100)

	require.Empty(t, f.db.LedgerRecords["hive:alice"],
		"reverse with no matching slash must not write any row")
}

func TestApplySafetySlashReverse_DropsOnUnsupportedAction(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-bad-action", 100, 0)

	priorReverseCount := 0
	for _, r := range f.db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			priorReverseCount++
		}
	}

	rec := safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-bad-action",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.SafetySlashReverseAction("nuke"),
		Amount:         10_000,
	}
	f.se.ApplySafetySlashReverseForTest(rec, "ctx", 110)

	postReverseCount := 0
	for _, r := range f.db.LedgerRecords["hive:alice"] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashConsensusReverse {
			postReverseCount++
		}
	}
	require.Equal(t, priorReverseCount, postReverseCount,
		"unsupported action must not write any reverse rows")
}
