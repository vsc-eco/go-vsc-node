package state_engine_test

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
)

// Consensus 0.2.0 delegated stake/unstake — core invariant test (ledger layer,
// gate forced via ConsensusParams.Delegated so the mechanics are exercised
// without standing up an election at version 0.2.0).
//
// Scenario: userA delegates consensus stake to operatorB. Required properties:
//  1. userA -> operatorB stake succeeds and records the per-edge delegation.
//  2. operatorB CANNOT unstake what userA delegated (no edge of its own) — the
//     headline requirement: the operator can never touch a delegator's stake.
//  3. userA over-unstake (more than delegated) is rejected.
//  4. userA can unstake exactly what it delegated.
func TestConsensusDelegatedUnstakeEntitlement(t *testing.T) {
	const userA = "hive:usera"
	const operatorB = "hive:operatorb"

	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		// userA funds the delegation; operatorB has no spendable hive of its own.
		userA: {{Account: userA, BlockHeight: 0, Hive: 100000}},
	})
	ls := ledgerSystem.New(balDb, newMockLedgerDb(), nil, newMockActionsDb(), nil)
	session := ls.NewEmptySession(ls.NewEmptyState(), 1)

	// 1. userA delegates 10.000 HIVE to operatorB.
	stake := session.ConsensusStake(ledgerSystem.ConsensusParams{
		Id:          "stake-1",
		From:        userA,
		To:          operatorB,
		Amount:      10000,
		BlockHeight: 1,
		Type:        "stake",
		Delegated:   true,
	})
	assert.True(t, stake.Ok, "userA->operatorB delegated stake should succeed: %s", stake.Msg)

	// 2. operatorB tries to unstake (To=operatorB): its OWN edge is 0, so even
	//    though the node's hive_consensus pool holds userA's 10.000, it is
	//    rejected. operatorB can never reach userA's delegation.
	opUnstake := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "unstake-op",
		From:          operatorB,
		To:            operatorB,
		Amount:        10000,
		BlockHeight:   2,
		Type:          "unstake",
		ElectionEpoch: 1,
		Delegated:     true,
	})
	assert.False(t, opUnstake.Ok,
		"operatorB MUST NOT be able to unstake userA's delegated stake")
	assert.Equal(t, "amount exceeds delegated stake", opUnstake.Msg)

	// 3. userA over-unstake (20.000 > 10.000 delegated) — rejected.
	over := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "unstake-over",
		From:          userA,
		To:            operatorB,
		Amount:        20000,
		BlockHeight:   2,
		Type:          "unstake",
		ElectionEpoch: 1,
		Delegated:     true,
	})
	assert.False(t, over.Ok, "userA over-unstake must be rejected")
	assert.Equal(t, "amount exceeds delegated stake", over.Msg)

	// 4. userA unstakes exactly the delegated amount — succeeds.
	ok := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "unstake-ok",
		From:          userA,
		To:            operatorB,
		Amount:        10000,
		BlockHeight:   2,
		Type:          "unstake",
		ElectionEpoch: 1,
		Delegated:     true,
	})
	assert.True(t, ok.Ok, "userA unstaking its own delegation should succeed: %s", ok.Msg)

	// 5. After fully unstaking, the edge is drained — a further unstake fails.
	again := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "unstake-again",
		From:          userA,
		To:            operatorB,
		Amount:        1,
		BlockHeight:   2,
		Type:          "unstake",
		ElectionEpoch: 1,
		Delegated:     true,
	})
	assert.False(t, again.Ok, "edge is drained; further unstake must be rejected")
}

// Legacy path (Delegated=false) must be byte-identical to pre-0.2.0: unstake
// authorizes against the signer's own hive_consensus, not a delegation edge.
func TestConsensusUnstakeLegacyUnchanged(t *testing.T) {
	const node = "hive:selfstaker"
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		node: {{Account: node, BlockHeight: 0, Hive: 50000, HIVE_CONSENSUS: 30000}},
	})
	ls := ledgerSystem.New(balDb, newMockLedgerDb(), nil, newMockActionsDb(), nil)
	session := ls.NewEmptySession(ls.NewEmptyState(), 1)

	// Legacy unstake of own bond up to hive_consensus succeeds.
	ok := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "legacy-ok",
		From:          node,
		To:            node,
		Amount:        30000,
		BlockHeight:   1,
		Type:          "unstake",
		ElectionEpoch: 1,
		Delegated:     false,
	})
	assert.True(t, ok.Ok, "legacy self-unstake within hive_consensus should succeed: %s", ok.Msg)

	// Legacy unstake beyond hive_consensus is rejected with the legacy message.
	over := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id:            "legacy-over",
		From:          node,
		To:            node,
		Amount:        999999,
		BlockHeight:   1,
		Type:          "unstake",
		ElectionEpoch: 1,
		Delegated:     false,
	})
	assert.False(t, over.Ok)
	assert.Equal(t, "insufficient balance", over.Msg)
}

// Pro-rata slashing (the team's chosen policy): every delegator to a slashed
// node loses the SAME fraction, independent of unstake order. Verified via the
// balance effects of an unstake (released HIVE = the hive_consensus debit).
//
// Setup: userA and userC each delegated 10.000 to operatorB (gross total 20.000,
// bond 20.000); operatorB was then slashed 50% -> bond 10.000, gross total still
// 20.000. Each unstake of a 10.000 gross edge must release 10.000*bond/total.
func TestConsensusDelegatedUnstakeProRataSlash(t *testing.T) {
	const userA = "hive:usera"
	const userC = "hive:userc"
	const opB = "hive:operatorb"

	ledgerMock := newMockLedgerDb()
	seed := []ledgerDb.LedgerRecord{
		{Id: "a#edge", Owner: ledgerSystem.DelegationEdgeKey(userA, opB), Asset: ledgerSystem.AssetDelegation, Amount: 10000, Type: "consensus_stake", BlockHeight: 1},
		{Id: "c#edge", Owner: ledgerSystem.DelegationEdgeKey(userC, opB), Asset: ledgerSystem.AssetDelegation, Amount: 10000, Type: "consensus_stake", BlockHeight: 1},
		{Id: "a#total", Owner: opB, Asset: ledgerSystem.AssetDelegationTotal, Amount: 10000, Type: "consensus_stake", BlockHeight: 1},
		{Id: "c#total", Owner: opB, Asset: ledgerSystem.AssetDelegationTotal, Amount: 10000, Type: "consensus_stake", BlockHeight: 1},
		{Id: "a#out", Owner: opB, Asset: "hive_consensus", Amount: 10000, Type: "consensus_stake", BlockHeight: 1},
		{Id: "c#out", Owner: opB, Asset: "hive_consensus", Amount: 10000, Type: "consensus_stake", BlockHeight: 1},
		// 50% slash of operatorB's bond
		{Id: "slash#1", Owner: opB, Asset: "hive_consensus", Amount: -10000, Type: ledgerSystem.LedgerTypeSafetySlashConsensus, BlockHeight: 1},
	}
	if err := ledgerMock.StoreLedger(seed...); err != nil {
		t.Fatal(err)
	}

	ls := ledgerSystem.New(newMockBalanceDb(nil), ledgerMock, nil, newMockActionsDb(), nil)
	session := ls.NewEmptySession(ls.NewEmptyState(), 1)

	const h = 10
	assert.Equal(t, int64(10000), session.GetBalance(opB, h, "hive_consensus"), "bond is 50%-slashed")
	assert.Equal(t, int64(20000), session.GetBalance(opB, h, ledgerSystem.AssetDelegationTotal), "gross total is slash-immune")

	// userA unstakes its full 10.000 gross edge -> released = 10000*10000/20000 = 5000.
	rA := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id: "u-a", From: userA, To: opB, Amount: 10000, BlockHeight: h,
		Type: "unstake", ElectionEpoch: 1, Delegated: true,
	})
	assert.True(t, rA.Ok, rA.Msg)
	assert.Equal(t, int64(5000), session.GetBalance(opB, h, "hive_consensus"),
		"bond drops by RELEASED (5000), not gross")
	assert.Equal(t, int64(0), session.GetBalance(ledgerSystem.DelegationEdgeKey(userA, opB), h, ledgerSystem.AssetDelegation),
		"edge drains by GROSS (10000)")
	assert.Equal(t, int64(10000), session.GetBalance(opB, h, ledgerSystem.AssetDelegationTotal),
		"gross total drops by GROSS (10000) — keeps the ratio constant")

	// userC unstakes its full 10.000 -> ratio still 5000/10000 = 0.5 -> released 5000.
	rC := session.ConsensusUnstake(ledgerSystem.ConsensusParams{
		Id: "u-c", From: userC, To: opB, Amount: 10000, BlockHeight: h,
		Type: "unstake", ElectionEpoch: 1, Delegated: true,
	})
	assert.True(t, rC.Ok, rC.Msg)
	assert.Equal(t, int64(0), session.GetBalance(opB, h, "hive_consensus"),
		"both delegators released an equal 5000 — order-independent, bond fully distributed")
}
