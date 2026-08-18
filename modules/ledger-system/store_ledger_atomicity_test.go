package ledgerSystem_test

import (
	"errors"
	"testing"

	test_utils "vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"

	"github.com/stretchr/testify/assert"
)

// StoreLedger is not atomic across records — it loops per record and returns on
// the first error, leaving earlier records written. True atomicity needs a Mongo
// transaction, which needs a replica set; the deployment runs standalone.
//
// What makes that survivable is the combination the callers now rely on: every
// record has a deterministic id, the write is an upsert, and every
// consensus-relevant caller fail-stops and retries. This pins that combination —
// a partial write followed by a retry converges on the COMPLETE set, with no
// record double-applied.
func TestStoreLedger_PartialWriteThenRetry_Converges(t *testing.T) {
	lDb := &test_utils.MockLedgerDb{LedgerRecords: map[string][]ledgerDb.LedgerRecord{}}

	debit := ledgerDb.LedgerRecord{
		Id: "tx#debit", Owner: "hive:alice", Amount: -100, Asset: "hive", Type: "safety_slash_consensus",
	}
	credit := ledgerDb.LedgerRecord{
		Id: "tx#credit", Owner: "system:reserve", Amount: 100, Asset: "hive", Type: "safety_slash_reserve",
	}

	// First attempt: the debit lands, the credit fails — the half-applied write.
	lDb.StoreErrs = []error{nil, errors.New("connection reset by peer")}
	err := lDb.StoreLedger(debit, credit)
	assert.Error(t, err, "the batch must report the failure")
	assert.Len(t, lDb.LedgerRecords["hive:alice"], 1, "the debit landed")
	assert.Empty(t, lDb.LedgerRecords["system:reserve"], "the credit did not — this is the half-apply")

	// The caller fail-stops and retries the SAME batch.
	assert.NoError(t, lDb.StoreLedger(debit, credit))

	assert.Len(t, lDb.LedgerRecords["hive:alice"], 1,
		"the already-written debit must be REWRITTEN by id, never appended twice")
	assert.Len(t, lDb.LedgerRecords["system:reserve"], 1,
		"and the missing credit must now be present")

	var total int64
	for _, recs := range lDb.LedgerRecords {
		for _, r := range recs {
			total += r.Amount
		}
	}
	assert.Equal(t, int64(0), total,
		"the pair must net to zero — a retry that double-applied the debit would not")
}
