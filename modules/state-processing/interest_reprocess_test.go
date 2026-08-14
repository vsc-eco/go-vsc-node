package state_engine_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// TestClaimHBDInterest_ReprocessDropsLegacyRow_NoDoubleCredit covers fix #1:
// a block first processed under the pre-#241 index-based id scheme
// (hbd_interest_<h>_<idx>, no '#') must NOT double-credit when reprocessed under
// the account-keyed scheme. ClaimHBDInterest drops the legacy row up front, so
// only the new account-keyed row remains. Without the delete, the mock would
// hold both rows (distinct ids) and the account's interest would be doubled.
func TestClaimHBDInterest_ReprocessDropsLegacyRow_NoDoubleCredit(t *testing.T) {
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})
	if lDb.LedgerRecords == nil {
		lDb.LedgerRecords = map[string][]ledgerDb.LedgerRecord{}
	}

	// Simulate a prior pre-#241 run: alice's interest for this claim was written
	// under the legacy index id, stamped at block_height blockHeight+1 = 201.
	lDb.LedgerRecords["hive:alice"] = append(lDb.LedgerRecords["hive:alice"], ledgerDb.LedgerRecord{
		Id:          "hbd_interest_200_0",
		BlockHeight: 201,
		Amount:      50,
		Asset:       "hbd_savings",
		Owner:       "hive:alice",
		Type:        "interest",
	})

	// Reprocess the same claim under the new binary.
	ls.ClaimHBDInterest(100, 200, 50, "0")

	interestRows := 0
	var total int64
	for _, r := range lDb.LedgerRecords["hive:alice"] {
		if r.Type != "interest" {
			continue
		}
		interestRows++
		total += r.Amount
		assert.Contains(t, r.Id, "#", "legacy no-'#' interest row must be gone after reprocess")
	}
	assert.Equal(t, 1, interestRows, "reprocess must not leave both legacy and new-scheme interest rows")
	assert.Equal(t, int64(50), total, "interest must not be double-credited on reprocess")
}

// TestClaimHBDInterest_TwoOpsSameBlock_DistinctIds covers fix #2: two
// interest_operation vops in one Hive block get distinct disambiguators (their
// vop indices, "0" and "1"), so their account-keyed ids differ and neither
// overwrites the other. The all-zero virtual-op trx_id the code previously used
// would have collided both onto one id (the second silently overwriting the
// first). The fix #1 legacy-row delete leaves account-keyed ('#') rows intact,
// so both distributions survive.
func TestClaimHBDInterest_TwoOpsSameBlock_DistinctIds(t *testing.T) {
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:           "hive:alice",
			BlockHeight:       100,
			HBD_SAVINGS:       1000,
			HBD_AVG:           0,
			HBD_CLAIM_HEIGHT:  100,
			HBD_MODIFY_HEIGHT: 100,
		}},
	})

	ls.ClaimHBDInterest(100, 200, 50, "0")
	ls.ClaimHBDInterest(100, 200, 50, "1")

	ids := map[string]bool{}
	for _, r := range lDb.LedgerRecords["hive:alice"] {
		if r.Type == "interest" {
			ids[r.Id] = true
		}
	}
	assert.Len(t, ids, 2, "two same-block interest ops must produce two distinct ids, not one overwriting the other")
	assert.Contains(t, ids, "hbd_interest_200_0#hive:alice")
	assert.Contains(t, ids, "hbd_interest_200_1#hive:alice")
}
