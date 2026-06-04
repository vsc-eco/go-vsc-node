package ledgerSystem_test

import (
	"math"
	"testing"

	"vsc-node/lib/test_utils"
	systemconfig "vsc-node/modules/common/system-config"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
)

// ---------------------------------------------------------------------------
// C1 regression tests — exercise the REAL ClaimHBDInterest end-to-end through
// the exported New(...) constructor + the test_utils mock DBs. These are the
// "proof inverted" tests for:
//
//   GV-L1: HBD-interest floor-division remainder accounting lie. The pre-fix
//          code records SaveClaim.Amount = amount (the nominal interest) even
//          though only amount-remainder was ever credited to the ledger. The
//          USER-DECISION fix is accounting-only: do NOT redistribute the dust
//          and do NOT write an extra ledger record — instead record the
//          TRUTHFUL amount actually distributed.
//   GV-L2: ClaimHBDInterest totalAvg int64 overflow → epoch distribution
//          skipped (the int64 accumulator wraps negative).
//
// Lesson from C2: a regression test that passes WITHOUT the fix is worthless.
// Each test below is constructed so that the pre-fix code FAILS the assertion:
//   - GV-L1: pre-fix SaveClaim.Amount == amount (100), but only 99 was
//     credited → the truthfulness assertion (SaveClaim.Amount == distributed
//     == 99) FAILS on pristine. The "no extra record" + "dust not credited"
//     assertions also pin the accounting-only semantics (they would FAIL
//     against a redistribution variant that credits the dust).
//   - GV-L2: pre-fix int64 accumulator wraps negative → every share is
//     skipped → zero credited but SaveClaim.Amount == amount → the assertion
//     that interest WAS credited FAILS.
// ---------------------------------------------------------------------------

// buildLedgerSystem wires the real ledgerSystem against in-memory mocks.
func buildLedgerSystem(balances *test_utils.MockBalanceDb) (
	ledgerSystem.LedgerSystem, *test_utils.MockLedgerDb, *test_utils.MockInterestClaimsDb,
) {
	ledgerDbMock := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	claimDbMock := &test_utils.MockInterestClaimsDb{}
	actionsDbMock := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	ls := ledgerSystem.New(balances, ledgerDbMock, claimDbMock, actionsDbMock, systemconfig.MocknetConfig())
	return ls, ledgerDbMock, claimDbMock
}

// addBalance seeds one account so that ClaimHBDInterest computes a known
// endingAvg. With A=B=1 and HBD_AVG=0, computeEndingAvg returns exactly
// HBD_SAVINGS, giving the test direct control over each account's weight.
func addBalance(db *test_utils.MockBalanceDb, account string, bh, hbdSavings uint64) {
	db.BalanceRecords[account] = []ledgerDb.BalanceRecord{{
		Account:           account,
		BlockHeight:       bh,
		HBD_AVG:           0,
		HBD_SAVINGS:       int64(hbdSavings),
		HBD_CLAIM_HEIGHT:  bh - 1, // B = bh - (bh-1) = 1
		HBD_MODIFY_HEIGHT: bh - 1, // A = bh - (bh-1) = 1
	}}
}

// sumInterestCredited totals every "interest" ledger record across all owners.
func sumInterestCredited(db *test_utils.MockLedgerDb) int64 {
	total := int64(0)
	for _, recs := range db.LedgerRecords {
		for _, r := range recs {
			if r.Type == "interest" {
				total += r.Amount
			}
		}
	}
	return total
}

// countInterestRecords counts every "interest" ledger record across all owners.
func countInterestRecords(db *test_utils.MockLedgerDb) int {
	n := 0
	for _, recs := range db.LedgerRecords {
		for _, r := range recs {
			if r.Type == "interest" {
				n++
			}
		}
	}
	return n
}

// hasAnyRemainderRecord reports whether any ledger record id contains the
// "_remainder" suffix that the (rejected) redistribution variant would have
// written. The accounting-only fix must write NONE.
func hasAnyRemainderRecord(db *test_utils.MockLedgerDb) bool {
	for _, recs := range db.LedgerRecords {
		for _, r := range recs {
			if len(r.Id) >= len("_remainder") &&
				r.Id[len(r.Id)-len("_remainder"):] == "_remainder" {
				return true
			}
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// GV-L1 — accounting-only: SaveClaim records the TRUTHFUL distributed total,
// the dust is NOT redistributed, and NO extra ledger record is written.
//
// 3 equal accounts, amount=100. Each floor share = floor(100 * w / 3w) = 33,
// so 99 is credited and the 1 milli-HBD remainder stays un-distributed.
// Pre-fix: SaveClaim.Amount == 100 (overstated) → the truthfulness assertion
// (== distributed == 99) FAILS on pristine.
// ---------------------------------------------------------------------------
func TestFixGVL1_SaveClaimRecordsTruthfulDistributed(t *testing.T) {
	const bh = uint64(1000)
	const amount = int64(100)

	balances := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	// Equal weights → each computeDistributeAmount = floor(100/3 share) = 33.
	addBalance(balances, "hive:a", bh, 1000)
	addBalance(balances, "hive:b", bh, 1000)
	addBalance(balances, "hive:c", bh, 1000)

	ls, ledgerDbMock, claimDbMock := buildLedgerSystem(balances)
	ls.ClaimHBDInterest(0, bh, amount, "tx-gvl1")

	// 3 equal accounts of 33 → exactly 99 credited; the 1-unit remainder is
	// left un-distributed (NOT redistributed).
	credited := sumInterestCredited(ledgerDbMock)
	const wantDistributed = int64(99)
	if credited != wantDistributed {
		t.Fatalf("GV-L1: total interest credited=%d, want=%d (floor shares only; dust left un-distributed)", credited, wantDistributed)
	}

	// Accounting-only: the dust must NOT be credited anywhere. Credited must be
	// strictly less than the nominal amount (this is the un-distributed dust).
	if credited >= amount {
		t.Fatalf("GV-L1 accounting-only: credited=%d must be < nominal amount=%d (the dust must stay un-distributed, not redistributed)", credited, amount)
	}

	// No extra ledger record (no remainder-recipient row) may be written.
	if hasAnyRemainderRecord(ledgerDbMock) {
		t.Fatalf("GV-L1 accounting-only: a *_remainder ledger record was written — the fix must NOT add an extra ledger record")
	}
	// Exactly N=3 interest records, one per account, nothing extra.
	if got := countInterestRecords(ledgerDbMock); got != 3 {
		t.Fatalf("GV-L1 accounting-only: expected exactly 3 interest records (one per account), got %d", got)
	}

	// THE load-bearing assertion (fails on pristine, where SaveClaim.Amount
	// == amount == 100): SaveClaim must record the TRUTHFUL distributed total.
	if len(claimDbMock.Claims) != 1 {
		t.Fatalf("GV-L1: expected exactly 1 SaveClaim, got %d", len(claimDbMock.Claims))
	}
	if claimDbMock.Claims[0].Amount != wantDistributed {
		t.Fatalf("GV-L1: SaveClaim.Amount=%d, want=%d (must equal what was ACTUALLY distributed, NOT the nominal amount=%d)", claimDbMock.Claims[0].Amount, wantDistributed, amount)
	}
	if claimDbMock.Claims[0].Amount == amount {
		t.Fatalf("GV-L1: SaveClaim.Amount overstated as nominal amount=%d while only %d was distributed", amount, wantDistributed)
	}
}

// GV-L1 — N=1: a single recipient takes the whole amount; no truncation is
// possible so distributed == amount and NO extra record is written. This case
// is behaviorally identical pre/post-fix (passes on pristine too — correct).
func TestFixGVL1_SingleAccountNoRemainder(t *testing.T) {
	const bh = uint64(3000)
	const amount = int64(100)

	balances := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	addBalance(balances, "hive:solo", bh, 1000)

	ls, ledgerDbMock, claimDbMock := buildLedgerSystem(balances)
	ls.ClaimHBDInterest(0, bh, amount, "tx-solo")

	if got := sumInterestCredited(ledgerDbMock); got != amount {
		t.Fatalf("GV-L1 N=1: credited=%d, want=%d (sole account gets the whole amount)", got, amount)
	}
	if hasAnyRemainderRecord(ledgerDbMock) {
		t.Fatalf("GV-L1 N=1: unexpected *_remainder record (remainder must be 0, accounting-only writes none anyway)")
	}
	if got := countInterestRecords(ledgerDbMock); got != 1 {
		t.Fatalf("GV-L1 N=1: expected exactly 1 interest record, got %d", got)
	}
	if claimDbMock.Claims[0].Amount != amount {
		t.Fatalf("GV-L1 N=1: SaveClaim.Amount=%d, want=%d (no truncation → distributed == amount)", claimDbMock.Claims[0].Amount, amount)
	}
}

// GV-L1 — N=0: no qualifying accounts. SaveClaim must record 0 (truthful — zero
// distributed), no ledger records, and the function must not panic.
func TestFixGVL1_NoAccounts(t *testing.T) {
	const bh = uint64(4000)
	const amount = int64(100)

	balances := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	// No balances seeded at all.

	ls, ledgerDbMock, claimDbMock := buildLedgerSystem(balances)
	ls.ClaimHBDInterest(0, bh, amount, "tx-empty")

	if got := sumInterestCredited(ledgerDbMock); got != 0 {
		t.Fatalf("GV-L1 N=0: credited=%d, want=0", got)
	}
	if hasAnyRemainderRecord(ledgerDbMock) {
		t.Fatalf("GV-L1 N=0: unexpected *_remainder record")
	}
	if got := countInterestRecords(ledgerDbMock); got != 0 {
		t.Fatalf("GV-L1 N=0: expected 0 interest records, got %d", got)
	}
	if len(claimDbMock.Claims) != 1 || claimDbMock.Claims[0].Amount != 0 {
		t.Fatalf("GV-L1 N=0: SaveClaim must record amount=0 (zero distributed), got %+v", claimDbMock.Claims)
	}
}

// ---------------------------------------------------------------------------
// GV-L2 — cross-account endingAvg sum exceeds int64 range. (KEPT unchanged in
// behavior from the prior build; only the GV-L1 part of the fix changed.)
//
// Two accounts, each with HBD_SAVINGS just over MaxInt64/2, so each individual
// endingAvg fits int64 but their SUM overflows. Pre-fix: the int64 accumulator
// wraps negative, computeDistributeAmount's old `== 0` guard lets the negative
// denominator through, every share comes out negative, the `> 0` filter skips
// them all → ZERO credited, yet SaveClaim recorded `amount`. After the fix the
// big.Int accumulator never wraps, so distribution proceeds normally.
// ---------------------------------------------------------------------------
func TestFixGVL2_TotalAvgOverflowStillDistributes(t *testing.T) {
	const bh = uint64(5000)
	const amount = int64(1_000_000)

	// Each account's endingAvg == HBD_SAVINGS (A=B=1, HBD_AVG=0).
	// Pick a value > MaxInt64/2 so two of them sum past MaxInt64.
	half := uint64(math.MaxInt64/2) + uint64(1_000_000) // > 2^62, fits int64

	balances := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	addBalance(balances, "hive:whale1", bh, half)
	addBalance(balances, "hive:whale2", bh, half)

	// Sanity: confirm the int64 sum really would overflow (negative), which is
	// the exact pre-fix corruption this test guards against.
	if int64(half)+int64(half) >= 0 {
		t.Fatalf("test setup invalid: int64 sum did not overflow (half=%d)", half)
	}

	ls, ledgerDbMock, claimDbMock := buildLedgerSystem(balances)
	ls.ClaimHBDInterest(0, bh, amount, "tx-gvl2")

	credited := sumInterestCredited(ledgerDbMock)
	if credited <= 0 {
		t.Fatalf("GV-L2 regressed: cross-account sum overflowed int64 and the whole epoch's distribution was skipped (credited=%d). big.Int accumulator must prevent this.", credited)
	}
	// Two equal whales → each gets floor(amount/2) = 500000, summing to exactly
	// amount. (No dust here because amount is even and weights are equal.)
	if credited != amount {
		t.Fatalf("GV-L2: credited=%d, want full amount=%d (two equal whales → 500000 each, no dust)", credited, amount)
	}
	// Accounting-only invariant: SaveClaim.Amount equals what was distributed.
	if len(claimDbMock.Claims) != 1 || claimDbMock.Claims[0].Amount != credited {
		t.Fatalf("GV-L2: SaveClaim.Amount=%d, want=%d (must reflect actual distribution, not be recorded while 0 was paid)", claimDbMock.Claims[0].Amount, credited)
	}
	if claimDbMock.Claims[0].ObservedApr <= 0 {
		t.Fatalf("GV-L2: observedApr=%v, want a sensible positive value (big.Float path)", claimDbMock.Claims[0].ObservedApr)
	}
}
