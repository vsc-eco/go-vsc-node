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
