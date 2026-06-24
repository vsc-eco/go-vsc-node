package state_engine_test

import (
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	governance "vsc-node/modules/governance"
	ledgerSystem "vsc-node/modules/ledger-system"
)

func recipientCredit(db *test_utils.MockLedgerDb, recipient string) int64 {
	var total int64
	for _, r := range db.LedgerRecords[recipient] {
		if r.Type == ledgerSystem.LedgerTypeReservePayoutCredit {
			total += r.Amount
		}
	}
	return total
}

func reserveCreditsTotal(db *test_utils.MockLedgerDb) int64 {
	var total int64
	for _, r := range db.LedgerRecords[params.ProtocolSlashReserveAccount] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashReserve {
			total += r.Amount
		}
	}
	return total
}

// reserveAvailable = reserve credits + payout debits (debits negative).
func reserveAvailable(db *test_utils.MockLedgerDb) int64 {
	var total int64
	for _, r := range db.LedgerRecords[params.ProtocolSlashReserveAccount] {
		if r.Type == ledgerSystem.LedgerTypeSafetySlashReserve ||
			r.Type == ledgerSystem.LedgerTypeReservePayoutDebit {
			total += r.Amount
		}
	}
	return total
}

// TestReservePayout_PrimitiveCapAndIdempotency exercises the ledger primitive
// directly: supply-conserving disbursement, idempotency on re-apply, the
// balance cap, and rejection once the reserve is empty.
func TestReservePayout_PrimitiveCapAndIdempotency(t *testing.T) {
	f := newChainOpFixture(t)
	f.fireSlash(t, "fund-1", 100, 0) // burnDelay 0 → 100_000 lands straight in the reserve
	require.EqualValues(t, 100_000, reserveCreditsTotal(f.db))

	// Pay 60k to victim.
	r := f.ls.ReservePayout(ledgerSystem.ReservePayoutParams{
		ProposalID: "p1", Recipient: "victim", Amount: 60_000, BlockHeight: 120,
	})
	require.True(t, r.Ok, r.Msg)
	require.EqualValues(t, 60_000, recipientCredit(f.db, "hive:victim"), "recipient credited spendable hive")
	require.EqualValues(t, 40_000, reserveAvailable(f.db), "reserve drawn down by the payout")

	// Idempotent: re-applying the same proposal does not double-pay.
	f.ls.ReservePayout(ledgerSystem.ReservePayoutParams{
		ProposalID: "p1", Recipient: "victim", Amount: 60_000, BlockHeight: 121,
	})
	require.EqualValues(t, 60_000, recipientCredit(f.db, "hive:victim"), "idempotent on proposal id")
	require.EqualValues(t, 40_000, reserveAvailable(f.db))

	// Cap: a second proposal requesting more than remaining is capped at 40k.
	r2 := f.ls.ReservePayout(ledgerSystem.ReservePayoutParams{
		ProposalID: "p2", Recipient: "victim", Amount: 999_999, BlockHeight: 122,
	})
	require.True(t, r2.Ok, r2.Msg)
	require.EqualValues(t, 100_000, recipientCredit(f.db, "hive:victim"), "second payout capped to remaining reserve")
	require.EqualValues(t, 0, reserveAvailable(f.db), "reserve fully drawn")

	// Empty: a further payout is rejected (cannot overdraw / mint).
	r3 := f.ls.ReservePayout(ledgerSystem.ReservePayoutParams{
		ProposalID: "p3", Recipient: "victim", Amount: 1, BlockHeight: 123,
	})
	require.False(t, r3.Ok, "payout from an empty reserve must be rejected")
}

// TestReservePayout_CreateVoteDisburses is the governance path: a witness
// proposes a payout (create == first vote), others approve by id, and on crossing
// 2/3 of the electorate (recipient excluded — victim is not a member, so the
// effective electorate is all 4 → threshold 3) the reserve disburses.
func TestReservePayout_CreateVoteDisburses(t *testing.T) {
	f, gov := wireRestoreFixture(t)
	f.fireSlash(t, "fund-1", 100, 0) // fund the reserve with 100_000
	require.EqualValues(t, 100_000, reserveCreditsTotal(f.db))

	amount := int64(60_000)
	reason := "theft-incident-x"
	createPayload := fmt.Sprintf(`{"recipient":"victim","amount":%d,"reason":%q}`, amount, reason)
	propID := governance.ReservePayoutProposalID("victim", amount, reason, "create-tx")
	votePayload := `{"id":"` + propID + `"}`

	// Create (alice's first vote) — 1 of 3.
	f.se.HandleReservePayoutCreateForTest([]byte(createPayload), "alice", "create-tx", 110)
	require.EqualValues(t, 0, recipientCredit(f.db, "hive:victim"), "no payout below threshold")

	// bob votes — 2 of 3.
	f.se.HandleReservePayoutVoteForTest([]byte(votePayload), "bob", "v-bob", 111)
	require.EqualValues(t, 0, recipientCredit(f.db, "hive:victim"))

	// carol votes — 3 of 3 → payout disburses.
	f.se.HandleReservePayoutVoteForTest([]byte(votePayload), "carol", "v-carol", 112)
	require.EqualValues(t, 60_000, recipientCredit(f.db, "hive:victim"), "threshold crossed → reserve payout")
	require.EqualValues(t, 40_000, reserveAvailable(f.db))

	p, ok, _ := gov.GetProposal(propID)
	require.True(t, ok)
	require.Equal(t, "applied", p.Status)

	// A late vote after apply is a no-op.
	f.se.HandleReservePayoutVoteForTest([]byte(votePayload), "dave", "v-dave", 113)
	require.EqualValues(t, 60_000, recipientCredit(f.db, "hive:victim"), "no double payout after applied")
}

// TestReservePayout_NonWitnessCreateIgnored: only electorate members may propose.
func TestReservePayout_NonWitnessCreateIgnored(t *testing.T) {
	f, gov := wireRestoreFixture(t)
	f.fireSlash(t, "fund-1", 100, 0)

	amount := int64(10_000)
	createPayload := fmt.Sprintf(`{"recipient":"victim","amount":%d,"reason":"x"}`, amount)
	propID := governance.ReservePayoutProposalID("victim", amount, "x", "create-tx")

	f.se.HandleReservePayoutCreateForTest([]byte(createPayload), "eve", "create-tx", 110)
	_, ok, _ := gov.GetProposal(propID)
	require.False(t, ok, "non-witness must not create a reserve payout proposal")
	require.EqualValues(t, 0, recipientCredit(f.db, "hive:victim"))
}

// TestReservePayout_VoteBeforeCreateIgnored: a vote for an unknown proposal id
// is dropped deterministically (no proposal, no payout).
func TestReservePayout_VoteBeforeCreateIgnored(t *testing.T) {
	f, _ := wireRestoreFixture(t)
	f.fireSlash(t, "fund-1", 100, 0)
	f.se.HandleReservePayoutVoteForTest([]byte(`{"id":"reserve_payout:deadbeef"}`), "bob", "v-bob", 110)
	require.EqualValues(t, 0, recipientCredit(f.db, "hive:victim"))
}
