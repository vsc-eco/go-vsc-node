package state_engine_test

import (
	"fmt"
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
)

// TestClaimHBDInterestRecordIdsAreDeterministic pins the interest ledger
// record id to the ACCOUNT rather than to iteration order.
//
// Why this harness is a faithful adversary: the production code derives the
// order from BalanceDb.GetAll, which walks a Mongo Distinct("account") — an
// operation with NO defined order. The mock walks a Go map, whose iteration
// order the runtime deliberately randomises on every range. Both are
// "unordered set of accounts", so a record id that depends on position is
// unstable under either.
//
// Pre-fix the id was "hbd_interest_<height>_<loopIndex>", so the SAME account
// received a DIFFERENT id from run to run (and from node to node). Since
// StoreLedger upserts on that id, an unstable id also means a replay writes
// the same payment under a second id instead of overwriting the first.
//
// Post-fix the id is "hbd_interest_<height>_<txId>#<account>", which is a pure
// function of the payment.
func TestClaimHBDInterestRecordIdsAreDeterministic(t *testing.T) {
	accounts := []string{
		"hive:alice", "hive:bob", "hive:carol",
		"hive:dave", "hive:erin", "hive:frank",
	}

	const (
		claimHeight = uint64(100)
		blockHeight = uint64(200)
		amount      = int64(6000)
		txID        = "abc123deadbeef"
		iterations  = 40
	)

	seed := func() map[string][]ledgerDb.BalanceRecord {
		m := make(map[string][]ledgerDb.BalanceRecord, len(accounts))
		for i, acct := range accounts {
			m[acct] = []ledgerDb.BalanceRecord{{
				Account: acct,
				// Distinct savings so the accounts are not interchangeable.
				HBD_SAVINGS:       int64(1000 * (i + 1)),
				BlockHeight:       claimHeight,
				HBD_AVG:           0,
				HBD_CLAIM_HEIGHT:  claimHeight,
				HBD_MODIFY_HEIGHT: claimHeight,
			}}
		}
		return m
	}

	// account -> id observed on the first iteration
	firstSeen := make(map[string]string)
	distinctIDsPerAccount := make(map[string]map[string]struct{})

	for iter := 0; iter < iterations; iter++ {
		ls, lDb, _ := newLedgerEnvWithClaims(seed())
		ls.ClaimHBDInterest(claimHeight, blockHeight, amount, txID)

		for _, acct := range accounts {
			recs := lDb.LedgerRecords[acct]
			if len(recs) != 1 {
				t.Fatalf("iter %d: account %s got %d interest records, want 1",
					iter, acct, len(recs))
			}
			id := recs[0].Id

			if distinctIDsPerAccount[acct] == nil {
				distinctIDsPerAccount[acct] = make(map[string]struct{})
			}
			distinctIDsPerAccount[acct][id] = struct{}{}

			if iter == 0 {
				firstSeen[acct] = id
				continue
			}
			if id != firstSeen[acct] {
				t.Errorf("NON-DETERMINISTIC id for %s: iteration 0 gave %q, iteration %d gave %q",
					acct, firstSeen[acct], iter, id)
			}
		}
	}

	// Every account must have produced exactly ONE distinct id across all runs.
	for _, acct := range accounts {
		if n := len(distinctIDsPerAccount[acct]); n != 1 {
			ids := make([]string, 0, n)
			for id := range distinctIDsPerAccount[acct] {
				ids = append(ids, id)
			}
			t.Errorf("account %s produced %d distinct ids across %d runs: %v",
				acct, n, iterations, ids)
		}
	}

	// Ids must be unique across accounts — a collision would make StoreLedger
	// upsert one account's interest on top of another's.
	seenID := make(map[string]string)
	for _, acct := range accounts {
		id := firstSeen[acct]
		if other, dup := seenID[id]; dup {
			t.Errorf("id collision: %s and %s both got %q", other, acct, id)
		}
		seenID[id] = acct
	}

	// And the id must actually carry the account, which is what makes it a
	// function of the payment rather than of iteration order.
	for _, acct := range accounts {
		want := fmt.Sprintf("hbd_interest_%d_%s#%s", blockHeight, txID, acct)
		if firstSeen[acct] != want {
			t.Errorf("id for %s = %q, want %q", acct, firstSeen[acct], want)
		}
	}

	t.Logf("stable ids across %d randomised iterations:", iterations)
	for _, acct := range accounts {
		t.Logf("  %-12s -> %s", acct, firstSeen[acct])
	}
}
