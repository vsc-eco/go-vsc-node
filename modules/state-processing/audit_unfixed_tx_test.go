package state_engine_test

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAuditUnfixed_26_PartialUnbondAllowsNearZeroBondWithCommitteeSeat —
// consensus audit MEDIUM #26 (partial unbond escapes MinStake floor while the
// account still occupies a committee seat).
//
// Precondition: ConsensusUnstake (modules/ledger-system/ledger_session.go:239)
// only checks that the account has enough hive_consensus to cover the unstake
// amount. There is no post-unstake check that the residual bond is still at
// least MinStake while the account is on the active committee. A committee
// member can therefore drain their bond down to one base unit and remain
// seated until the next election rotates them out — they keep voting / signing
// weight while having effectively no skin in the game.
//
// We pin this at the LedgerSession layer (per the test-pattern guidance — the
// underlying ConsensusUnstake has no MinStake awareness today), seeding the
// account with HIVE_CONSENSUS = minStake directly in the balance snapshot.
//
// Differential: today the unstake of (minStake - 1) returns Ok=true. Post-fix
// the LedgerSession (or the TxConsensusUnstake wrapper) should consult
// committee membership and reject any unstake that would drop a seated member
// below MinStake until they're rotated out (or until they unstake the entire
// remaining bond as a "leave committee" intent).
func TestAuditUnfixed_26_PartialUnbondAllowsNearZeroBondWithCommitteeSeat(t *testing.T) {
	// Use a non-trivial MinStake so "minStake - 1" is a meaningful positive
	// amount (mocknet's MinStake=1 would collapse to a 0-amount unstake which
	// the amount<=0 guard already rejects — that's not the bug under test).
	const minStake int64 = 1000

	ledgerDbMock := newMockLedgerDb()
	balanceDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:        "hive:alice",
			BlockHeight:    0,
			HIVE_CONSENSUS: minStake, // already bonded at exactly MinStake
		}},
	})
	actionsDb := newMockActionsDb()

	state := &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     1,
		LedgerDb:        ledgerDbMock,
		BalanceDb:       balanceDb,
		ActionDb:        actionsDb,
	}
	session := ledgerSystem.NewSession(state)

	// Sanity: alice is exactly at MinStake before the unbond.
	pre := session.GetBalance("hive:alice", 1, "hive_consensus")
	require.Equal(t, minStake, pre, "precondition: alice seeded at MinStake")

	// Partial unbond that leaves alice with exactly 1 unit of hive_consensus —
	// far below MinStake. While alice is still on the active committee, this
	// should be rejected post-fix; today it succeeds.
	res := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "audit-26-unstake",
		From:          "hive:alice",
		To:            "hive:alice",
		Amount:        minStake - 1, // leaves 1 base unit
		BlockHeight:   1,
		Type:          "unstake",
		ElectionEpoch: 10,
	})

	// Current (buggy) behavior: the unstake succeeds with no MinStake-vs-seat
	// post-check. Comment: post-fix should reject unbond that would drop a
	// committee member below MinStake until they're removed from the committee
	// (or require a full-balance "leave" unstake).
	assert.True(t, res.Ok,
		"audit #26: ConsensusUnstake currently accepts a partial unbond that drops "+
			"a committee member below MinStake (Msg=%q)", res.Msg)
	assert.Equal(t, "success", res.Msg,
		"audit #26: expected current 'success' msg from the unguarded path")
}

// TestAuditUnfixed_44_OneWayDelegationNoRevocation — consensus audit MEDIUM
// #44 (consensus stake is one-way: alice can stake TO bob, but only bob can
// unstake, and there is no `revoke_consensus_stake` op).
//
// Precondition: TxConsensusStake (modules/state-processing/transactions.go:
// 690-735) passes whatever (From, To) pair the signer supplies straight to
// LedgerSession.ConsensusStake, which debits From's `hive` and credits To's
// `hive_consensus` (modules/ledger-system/utils.go:154-170). There is no
// check that To == From at stake time, and TxConsensusUnstake only accepts
// the unstake from the account that currently *holds* the hive_consensus
// balance — i.e. the delegate, not the original delegator.
//
// The economic effect: alice can permanently lock her HIVE into bob's voting
// bond. Bob can unstake (and presumably keep the proceeds — there is no
// "return to original From" semantics anywhere in ConsensusUnstake), and
// alice has no recourse. There is no `revoke_consensus_stake` op in the
// transaction grammar either.
//
// We pin this at the LedgerSession layer with the same fixture pattern as
// review2_consensus_stake_asset_test.go (the Tx wrapper just forwards the
// (From, To) pair to the ledger; there is no additional Tx-level guard).
//
// Differential — post-fix this test would flip its asserts after either:
//   (a) ConsensusStake / TxConsensusStake requires To == From at stake time, OR
//   (b) the transaction grammar gains a `revoke_consensus_stake` op that lets
//       the original delegator pull their bond back out of the delegate.
func TestAuditUnfixed_44_OneWayDelegationNoRevocation(t *testing.T) {
	ledgerDbMock := newMockLedgerDb()
	balanceDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:alice": {{
			Account:     "hive:alice",
			BlockHeight: 0,
			Hive:        10_000, // alice has HIVE to delegate
		}},
		// bob holds a pre-existing HIVE_CONSENSUS balance representing the
		// "post-settlement" view of a delegated stake from alice. Without this
		// the test would conflate "no revocation" with "stake oplog hasn't
		// been settled yet" — IngestOplog (which actually credits
		// hive_consensus) runs at block-end, not inside the session. Seeding
		// bob directly mirrors the steady state the bug describes: alice
		// already delegated, the credit landed on bob, and now alice tries to
		// revoke.
		"hive:bob": {{
			Account:        "hive:bob",
			BlockHeight:    0,
			HIVE_CONSENSUS: 5_000,
		}},
	})
	actionsDb := newMockActionsDb()

	newSession := func() ledgerSystem.LedgerSession {
		state := &ledgerSystem.LedgerState{
			Oplog:           make([]ledgerSystem.OpLogEvent, 0),
			VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
			GatewayBalances: make(map[string]uint64),
			BlockHeight:     1,
			LedgerDb:        ledgerDbMock,
			BalanceDb:       balanceDb,
			ActionDb:        actionsDb,
		}
		return ledgerSystem.NewSession(state)
	}

	// Step 1: alice can stake TO bob — no To==From check.
	stake := newSession().ConsensusStake(ledgerSystem.ConsensusParams{
		Id:          "audit-44-stake",
		From:        "hive:alice",
		To:          "hive:bob",
		Amount:      1_000,
		BlockHeight: 1,
		Type:        "stake",
	})
	assert.True(t, stake.Ok,
		"audit #44: ConsensusStake currently accepts From=alice To=bob with no To==From check (Msg=%q)", stake.Msg)

	// Step 2: alice — the original delegator — tries to unstake. She has no
	// hive_consensus balance of her own (only bob does), so the unstake fails
	// with "insufficient balance". This is the exact "no revocation path"
	// shape: alice's HIVE was debited at stake time but she has no way to pull
	// it back through ConsensusUnstake.
	aliceUnstake := newSession().ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "audit-44-alice-unstake",
		From:          "hive:alice",
		To:            "hive:alice",
		Amount:        1_000,
		BlockHeight:   1,
		Type:          "unstake",
		ElectionEpoch: 10,
	})
	assert.False(t, aliceUnstake.Ok,
		"audit #44: alice (original delegator) must not be able to ConsensusUnstake — she holds no hive_consensus")
	assert.Equal(t, "insufficient balance", aliceUnstake.Msg,
		"audit #44: expected ledger rejection at the hive_consensus balance check")

	// Step 3: bob (the delegate) can unstake the delegated bond — he holds
	// the hive_consensus balance, so the ledger lets it through. This is the
	// economic capture: bob keeps the HIVE that was originally alice's.
	bobUnstake := newSession().ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "audit-44-bob-unstake",
		From:          "hive:bob",
		To:            "hive:bob",
		Amount:        1_000,
		BlockHeight:   1,
		Type:          "unstake",
		ElectionEpoch: 10,
	})
	assert.True(t, bobUnstake.Ok,
		"audit #44: bob (delegate) can unstake the bond that was originally alice's (Msg=%q)", bobUnstake.Msg)

	// Post-fix the differential flips: either (a) Step 1 fails ("self
	// delegation only") so the bond never lands on bob in the first place,
	// OR (b) a new revoke_consensus_stake op succeeds for alice in Step 2.
	// The Tx-level wrapper in transactions.go:690-735 is the natural place
	// for fix (a); fix (b) needs a new TxRevokeConsensusStake type.
	_ = stateEngine.TxConsensusStake{} // anchor to keep the reference live
}
