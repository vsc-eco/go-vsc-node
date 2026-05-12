package state_engine_test

import (
	"strconv"
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
func (f *fakeChainOpTxDb) FindTransactions(ids []string, id *string, account *string, contract *string, status *dbTransactions.TransactionStatus, byType []string, ledgerToFrom *string, ledgerTypes []string, fromBlock *uint64, toBlock *uint64, offset int, limit int) ([]dbTransactions.TransactionRecord, error) {
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

	ls := ledgerSystem.New(balDb, lDb, nil, aDb)
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

func (f *chainOpFixture) claimRowsForVictim(victim string) []ledgerDb.LedgerRecord {
	out := []ledgerDb.LedgerRecord{}
	for _, r := range f.db.LedgerRecords[params.ProtocolSlashRestitutionClaimsAccount] {
		if r.Type == ledgerSystem.LedgerTypeSafetyRestitutionClaim && r.From == victim {
			out = append(out, r)
		}
	}
	return out
}

// --- vsc.restitution_claim handler tests ------------------------------------

func TestApplyRestitutionClaim_HappyPath_NoHarmProof(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-1", 100, 0)

	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-1",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-1",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       30_000,
	}
	f.se.ApplyRestitutionClaimForTest(rec, "carrying-tx-1", 110)

	rows := f.claimRowsForVictim("hive:bob")
	require.Len(t, rows, 1, "happy path with no harm proof should enqueue a single claim row")
	require.Equal(t, int64(30_000), rows[0].Amount)
	require.Equal(t, "safety_restitution_claim#claim-1", rows[0].Id)
}

func TestApplyRestitutionClaim_DropsOnMissingSlashRow(t *testing.T) {
	f := newChainOpFixture(t)

	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-x",
		VictimAccount:  "bob",
		SlashTxID:      "no-such-tx",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       1_000,
	}
	f.se.ApplyRestitutionClaimForTest(rec, "ctx", 100)
	require.Empty(t, f.claimRowsForVictim("hive:bob"),
		"missing safety_slash_consensus row must drop the claim")
}

func TestApplyRestitutionClaim_RejectsLossExceedingHeadroom(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-2", 100, 0)

	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-overflow",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-2",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       250_000,
	}
	f.se.ApplyRestitutionClaimForTest(rec, "ctx", 110)
	require.Empty(t, f.claimRowsForVictim("hive:bob"),
		"loss exceeding slash amount must drop the claim")
}

func TestApplyRestitutionClaim_RejectsNonPositiveLoss(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-3", 100, 0)

	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-zero",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-3",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       0,
	}
	f.se.ApplyRestitutionClaimForTest(rec, "ctx", 110)
	require.Empty(t, f.claimRowsForVictim("hive:bob"))
}

func TestApplyRestitutionClaim_HarmProof_VerifiedClaimEnqueues(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-harm", 100, 0)

	anchoredID := "slash-tx-harm"
	pending := dbTransactions.TransactionStatusIncluded
	f.tx.records["victim-tx"] = &dbTransactions.TransactionRecord{
		Id:         "victim-tx",
		Status:     pending,
		AnchoredId: &anchoredID,
	}
	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-harm-1",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-harm",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       40_000,
		VictimTxID:     "victim-tx",
	}
	f.se.ApplyRestitutionClaimForTest(rec, "ctx", 110)
	require.Len(t, f.claimRowsForVictim("hive:bob"), 1,
		"verified harm proof should enqueue the claim")
}

func TestApplyRestitutionClaim_HarmProof_DropsOnMismatchedAnchorId(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-harm", 100, 0)

	wrongAnchor := "different-anchor-tx"
	pending := dbTransactions.TransactionStatusIncluded
	f.tx.records["victim-tx"] = &dbTransactions.TransactionRecord{
		Id:         "victim-tx",
		Status:     pending,
		AnchoredId: &wrongAnchor,
	}
	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-harm-2",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-harm",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       40_000,
		VictimTxID:     "victim-tx",
	}
	f.se.ApplyRestitutionClaimForTest(rec, "ctx", 110)
	require.Empty(t, f.claimRowsForVictim("hive:bob"),
		"mismatched anchored id must drop the claim")
}

func TestApplyRestitutionClaim_HarmProof_DropsOnConfirmedVictim(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-harm", 100, 0)

	anchoredID := "slash-tx-harm"
	confirmed := dbTransactions.TransactionStatusConfirmed
	f.tx.records["victim-tx"] = &dbTransactions.TransactionRecord{
		Id:         "victim-tx",
		Status:     confirmed,
		AnchoredId: &anchoredID,
	}
	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-harm-3",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-harm",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       40_000,
		VictimTxID:     "victim-tx",
	}
	f.se.ApplyRestitutionClaimForTest(rec, "ctx", 110)
	require.Empty(t, f.claimRowsForVictim("hive:bob"),
		"confirmed victim tx is no harm; claim must drop")
}

func TestApplyRestitutionClaim_HarmProof_DropsOnMissingVictimRecord(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-harm", 100, 0)

	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-harm-4",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-harm",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       40_000,
		VictimTxID:     "missing-tx",
	}
	f.se.ApplyRestitutionClaimForTest(rec, "ctx", 110)
	require.Empty(t, f.claimRowsForVictim("hive:bob"),
		"missing victim tx record (unverifiable proof) must drop")
}

func TestApplyRestitutionClaim_Idempotent_SameClaimID(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-tx-id", 100, 0)

	rec := safetyslash.RestitutionClaimRecord{
		ClaimID:        "claim-dup",
		VictimAccount:  "bob",
		SlashTxID:      "slash-tx-id",
		SlashedAccount: "alice",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		LossHive:       25_000,
	}
	for i := 0; i < 3; i++ {
		f.se.ApplyRestitutionClaimForTest(rec, "ctx-"+strconv.Itoa(i), uint64(110+i))
	}
	rows := f.claimRowsForVictim("hive:bob")
	require.Len(t, rows, 1, "duplicate ClaimID applies must upsert one row")
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
	f := newChainOpFixture(t)
	f.fireSlash(t, "slash-rev", 100, 0)

	rec := safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-rev",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionReverse,
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
	f.fireSlash(t, "slash-2x", 100, 0)

	f.se.ApplySafetySlashReverseForTest(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-2x",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionReverse,
		Amount:         60_000,
	}, "ctx-1", 110)

	f.se.ApplySafetySlashReverseForTest(safetyslash.SafetySlashReverseRecord{
		SlashTxID:      "slash-2x",
		EvidenceKind:   safetyslash.EvidenceVSCDoubleBlockSign,
		SlashedAccount: "alice",
		Action:         safetyslash.ReverseActionReverse,
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
