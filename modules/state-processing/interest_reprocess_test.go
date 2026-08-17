package state_engine_test

import (
	"strings"
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

// TestClaimHBDInterest_SkippedAccount_KeepsLegacyRow is the regression test for
// the scoped legacy-row delete.
//
// The delete used to be unconditional for the whole block: every legacy
// (no-'#') interest row at the height was dropped up front, and only accounts
// that survived to the write loop got a replacement. An account filtered out
// earlier — endingAvg < 1 (ledger_system.go), an overflow guard, or a
// floor-division share of 0 — therefore had its historical interest credit
// DELETED with nothing written back, silently lowering its hbd_savings. That is
// exactly the shape of the over-debits that left ten mainnet accounts with
// negative balances, and it would have fired on the first reprocess of an old
// interest block.
//
// alice is credited (legacy row must be replaced by the account-keyed row).
// bob has zero savings and zero avg, so he is skipped — his legacy row must
// SURVIVE untouched.
func TestClaimHBDInterest_SkippedAccount_KeepsLegacyRow(t *testing.T) {
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account: "hive:alice", BlockHeight: 100, HBD_SAVINGS: 1000,
			HBD_AVG: 0, HBD_CLAIM_HEIGHT: 100, HBD_MODIFY_HEIGHT: 100,
		}},
		// Zero savings and zero accumulated average -> endingAvg < 1 -> skipped.
		"hive:bob": {{
			Account: "hive:bob", BlockHeight: 100, HBD_SAVINGS: 0,
			HBD_AVG: 0, HBD_CLAIM_HEIGHT: 100, HBD_MODIFY_HEIGHT: 100,
		}},
	})
	if lDb.LedgerRecords == nil {
		lDb.LedgerRecords = map[string][]ledgerDb.LedgerRecord{}
	}

	// Both accounts were paid under the pre-#241 index-based id scheme.
	lDb.LedgerRecords["hive:alice"] = append(lDb.LedgerRecords["hive:alice"], ledgerDb.LedgerRecord{
		Id: "hbd_interest_200_0", BlockHeight: 201, Amount: 50,
		Asset: "hbd_savings", Owner: "hive:alice", Type: "interest",
	})
	lDb.LedgerRecords["hive:bob"] = append(lDb.LedgerRecords["hive:bob"], ledgerDb.LedgerRecord{
		Id: "hbd_interest_200_1", BlockHeight: 201, Amount: 7,
		Asset: "hbd_savings", Owner: "hive:bob", Type: "interest",
	})

	ls.ClaimHBDInterest(100, 200, 50, "0")

	// alice: exactly one interest row, account-keyed, no double credit.
	aliceRows, aliceTotal := 0, int64(0)
	for _, r := range lDb.LedgerRecords["hive:alice"] {
		if r.Type == "interest" {
			aliceRows++
			aliceTotal += r.Amount
			assert.Contains(t, r.Id, "#", "alice's legacy row must be replaced by the account-keyed row")
		}
	}
	assert.Equal(t, 1, aliceRows, "alice must not keep both legacy and new rows")
	assert.Equal(t, int64(50), aliceTotal, "alice must not be double-credited")

	// bob: skipped by the distribution, so his legacy credit must still be there.
	bobRows, bobTotal := 0, int64(0)
	for _, r := range lDb.LedgerRecords["hive:bob"] {
		if r.Type == "interest" {
			bobRows++
			bobTotal += r.Amount
		}
	}
	assert.Equal(t, 1, bobRows,
		"a skipped account's legacy interest row must NOT be deleted — nothing replaces it")
	assert.Equal(t, int64(7), bobTotal,
		"deleting a skipped account's credit silently debits it (the negative-balance bug)")
}

// TestClaimHBDInterest_SharedOwner_SkippedAccountKeepsLegacyRow covers the
// owner-collision hole in the scoped delete.
//
// Legacy interest rows carry only `owner`, never the account, so two accounts
// that map to the SAME owner are indistinguishable in the delete:
// system:fr_balance credits the DAO wallet, and the DAO wallet is itself a
// real balance-bearing account (76 balance rows on mainnet). Scoping the
// delete purely by owner would drop BOTH legacy rows while writing only the
// one replacement that qualified — destroying a credit, which is precisely
// what the scoping exists to prevent. Any owner with a skipped candidate is
// therefore excluded from the delete entirely.
func TestClaimHBDInterest_SharedOwner_SkippedAccountKeepsLegacyRow(t *testing.T) {
	ls, lDb, _ := newLedgerEnvWithClaims(map[string][]ledgerDb.BalanceRecord{
		// Credited: produces a replacement row owned by the DAO wallet.
		"system:fr_balance": {{
			Account: "system:fr_balance", BlockHeight: 100, HBD_SAVINGS: 1000,
			HBD_AVG: 0, HBD_CLAIM_HEIGHT: 100, HBD_MODIFY_HEIGHT: 100,
		}},
		// Skipped (zero savings -> endingAvg < 1) but shares the SAME owner.
		"hive:vsc.dao": {{
			Account: "hive:vsc.dao", BlockHeight: 100, HBD_SAVINGS: 0,
			HBD_AVG: 0, HBD_CLAIM_HEIGHT: 100, HBD_MODIFY_HEIGHT: 100,
		}},
	})
	if lDb.LedgerRecords == nil {
		lDb.LedgerRecords = map[string][]ledgerDb.LedgerRecord{}
	}
	// A prior pre-#241 run paid the DAO wallet under the legacy id scheme.
	lDb.LedgerRecords["hive:vsc.dao"] = append(lDb.LedgerRecords["hive:vsc.dao"],
		ledgerDb.LedgerRecord{
			Id: "hbd_interest_200_7", BlockHeight: 201, Amount: 42,
			Asset: "hbd_savings", Owner: "hive:vsc.dao", Type: "interest",
		})

	ls.ClaimHBDInterest(100, 200, 50, "0")

	var legacyTotal int64
	legacyRows := 0
	for _, r := range lDb.LedgerRecords["hive:vsc.dao"] {
		if r.Type == "interest" && !strings.Contains(r.Id, "#") {
			legacyRows++
			legacyTotal += r.Amount
		}
	}
	assert.Equal(t, 1, legacyRows,
		"the DAO wallet's legacy row must survive: it shares an owner with a credited "+
			"account but was itself skipped, so nothing replaces it")
	assert.Equal(t, int64(42), legacyTotal, "its credit must not be destroyed")
}
