package ledgerSystem_test

import (
	"testing"

	test_utils "vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
)

// ★ B9 — a crash and replay must not SHRINK a safety slash.
//
// safetyEvidenceSeen is an in-process map with no persistence and no
// rehydration (unlike the double-sign map), so after a restart the same
// evidence is admitted again and SafetySlashConsensusBond re-runs. Its doc
// comment says that is safe because ids are deterministic and Mongo upserts —
// true for double-debiting, silent about amount drift:
//
//	the bond is a SNAPSHOT read, slashAmt is derived from it and capped at it,
//	and the debit id is fixed. Once the first debit is folded into a snapshot
//	the replay's read can see, the second pass computes a SMALLER slash and
//	OVERWRITES the correct larger debit.
//
// The punishment silently shrinks, the row is not versioned, and nothing
// distinguishes it from a normal duplicate-evidence no-op.
func TestSafetySlash_ReplayAfterSnapshotFold_DoesNotShrinkTheDebit(t *testing.T) {
	balDb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{}}
	lDb := &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledgerDb.LedgerRecord{}}
	aDb := &test_utils.MockActionsDb{Actions: map[string]ledgerDb.ActionRecord{}}
	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)

	const acct = "hive:baddie"
	// Full bond at the slash height.
	balDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account: acct, BlockHeight: 1000, HIVE_CONSENSUS: 1_000_000,
	}}

	p := ledgerSystem.SafetySlashConsensusParams{
		Account:      acct,
		TxID:         "tx-evidence-1",
		EvidenceKind: "double_sign",
		SlashBps:     1000, // 10%
		BlockHeight:  1000,
	}

	first := ls.SafetySlashConsensusBond(p)
	assert.True(t, first.Ok, "first slash must apply")

	debitID := p.TxID + "#safety_slash#" + p.EvidenceKind + "#consensus_debit#" + acct
	find := func() *ledgerDb.LedgerRecord {
		for _, r := range lDb.LedgerRecords[acct] {
			if r.Id == debitID {
				return &r
			}
		}
		return nil
	}
	orig := find()
	if !assert.NotNil(t, orig, "the debit must be written") {
		return
	}
	assert.Equal(t, int64(-100_000), orig.Amount, "10% of 1,000,000")

	// The crash-and-replay condition: the debit has been folded into a snapshot
	// that the replay's bond read can see, so the bond now reads 900,000 and a
	// recompute would slash only 90,000 — overwriting the correct 100,000.
	balDb.BalanceRecords[acct] = append(balDb.BalanceRecords[acct], ledgerDb.BalanceRecord{
		Account: acct, BlockHeight: 1000, HIVE_CONSENSUS: 900_000,
	})

	ls.SafetySlashConsensusBond(p) // same evidence, replayed

	after := find()
	if assert.NotNil(t, after, "the debit must still exist after replay") {
		assert.Equal(t, int64(-100_000), after.Amount,
			"a replay must NOT shrink the slash — recomputing against a bond that "+
				"already reflects the debit overwrites the correct amount with a smaller one")
	}
}

// ★ REVIEW FINDING — the replay guard must require the COMPLETE record set.
//
// StoreLedger is non-atomic per record, so a crash between the hive_consensus
// debit and its paired hive-asset leg (reserve, or pending-burn under a
// challenge window) leaves the bond debited with the value nowhere. Keying the
// guard on the debit alone would short-circuit every future replay before that
// second row could ever be written — and the hive_consensus filter cannot even
// see the missing leg.
func TestSafetySlash_HalfAppliedWrite_IsCompletedOnReplay(t *testing.T) {
	balDb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{}}
	lDb := &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledgerDb.LedgerRecord{}}
	aDb := &test_utils.MockActionsDb{Actions: map[string]ledgerDb.ActionRecord{}}
	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)

	const acct = "hive:baddie"
	balDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account: acct, BlockHeight: 1000, HIVE_CONSENSUS: 1_000_000,
	}}
	p := ledgerSystem.SafetySlashConsensusParams{
		Account: acct, TxID: "tx-ev", EvidenceKind: "double_sign",
		SlashBps: 1000, BlockHeight: 1000,
	}

	ls.SafetySlashConsensusBond(p)

	// Simulate the crash: the debit landed, the reserve leg did not.
	base := p.TxID + "#safety_slash#" + p.EvidenceKind
	reserveID := base + "#reserve#" + acct
	for owner, recs := range lDb.LedgerRecords {
		kept := make([]ledgerDb.LedgerRecord, 0, len(recs))
		for _, r := range recs {
			if r.Id == reserveID {
				continue
			}
			kept = append(kept, r)
		}
		lDb.LedgerRecords[owner] = kept
	}

	// Replay must NOT short-circuit — it has to complete the missing leg.
	ls.SafetySlashConsensusBond(p)

	var found bool
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.Id == reserveID {
				found = true
			}
		}
	}
	assert.True(t, found,
		"a half-applied slash must be completed on replay — guarding on the debit "+
			"alone would strand the value with the bond already debited")
}

// ★ REVIEW PASS 7 — completing a half-applied slash must not bail out on the
// bond guards.
//
// By the time the companion leg is being completed, the bond legitimately reads
// 0: the debit that DID land is exactly what drove it there. The normal path's
// "zero consensus bond" and "slash rounds to zero" guards both RETURN, so
// falling through to them abandoned the completion before the recorded amount
// was ever applied — leaving the reserve/pending-burn leg unwritten forever
// with the bond already debited.
func TestSafetySlash_CompletesCompanion_EvenWhenBondNowReadsZero(t *testing.T) {
	balDb := &test_utils.MockBalanceDb{BalanceRecords: map[string][]ledgerDb.BalanceRecord{}}
	lDb := &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledgerDb.LedgerRecord{}}
	aDb := &test_utils.MockActionsDb{Actions: map[string]ledgerDb.ActionRecord{}}
	ls := ledgerSystem.New(balDb, lDb, nil, aDb, nil)

	const acct = "hive:baddie"
	balDb.BalanceRecords[acct] = []ledgerDb.BalanceRecord{{
		Account: acct, BlockHeight: 1000, HIVE_CONSENSUS: 1_000_000,
	}}
	p := ledgerSystem.SafetySlashConsensusParams{
		Account: acct, TxID: "tx-zero", EvidenceKind: "double_sign",
		SlashBps:    10000, // 100% — the bond ends at zero
		BlockHeight: 1000,
	}

	ls.SafetySlashConsensusBond(p)

	base := p.TxID + "#safety_slash#" + p.EvidenceKind
	reserveID := base + "#reserve#" + acct
	// Crash shape: the debit landed, the reserve leg did not.
	for owner, recs := range lDb.LedgerRecords {
		kept := make([]ledgerDb.LedgerRecord, 0, len(recs))
		for _, r := range recs {
			if r.Id != reserveID {
				kept = append(kept, r)
			}
		}
		lDb.LedgerRecords[owner] = kept
	}
	// And the bond snapshot now reflects the debit — it reads zero.
	balDb.BalanceRecords[acct] = append(balDb.BalanceRecords[acct], ledgerDb.BalanceRecord{
		Account: acct, BlockHeight: 1000, HIVE_CONSENSUS: 0,
	})

	ls.SafetySlashConsensusBond(p)

	var found bool
	var amt int64
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			if r.Id == reserveID {
				found, amt = true, r.Amount
			}
		}
	}
	assert.True(t, found,
		"the companion leg must be completed even though the bond now reads zero — "+
			"the guards must not bail out before the recorded amount is applied")
	assert.Equal(t, int64(1_000_000), amt, "and it must carry the originally recorded amount")
}
