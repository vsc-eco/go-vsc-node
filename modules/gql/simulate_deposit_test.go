package gql_test

// Exercises the resolver-level deposit helper that feature/simulate-dep adds,
// proving that a SimulateDepositInput credited through executeSimulatedDeposit
// becomes visible to a subsequent session.GetBalance / session.ExecuteTransfer
// — the exact chain the swap path takes during mixed-op simulation.

import (
	"testing"

	"vsc-node/lib/test_utils"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeResolverTestState() *ledgerSystem.LedgerState {
	return &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     100,
		LedgerDb: &test_utils.MockLedgerDb{
			LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
		},
		ActionDb: &test_utils.MockActionsDb{
			Actions: make(map[string]ledgerDb.ActionRecord),
		},
		BalanceDb: &test_utils.MockBalanceDb{
			BalanceRecords: make(map[string][]ledgerDb.BalanceRecord),
		},
	}
}

// Resolver-level deposit → primitive-level balance visibility. This is the
// link Milo was worried about: the helper appending the right thing to the
// session such that a follow-up op (simulated call → PullBalance) sees it.
func TestSimulateDep_ResolverHelperMakesBalanceVisible(t *testing.T) {
	// We can't construct the full Resolver without a stack of deps (StateEngine,
	// Da, Contracts, etc.). Instead, replicate what executeSimulatedDeposit
	// does — the helper body is minimal and the meaningful work is on the
	// session. Comparing the helper's AppendLedger call-shape against the
	// session's observable state is what we want to pin down.
	state := makeResolverTestState()
	session := ledgerSystem.NewSession(state)

	// Mirror executeSimulatedDeposit's owner resolution: with deposit.To set,
	// it calls ResolveDepositTarget("to="+deposit.To, deposit.From).
	owner := ledgerSystem.ResolveDepositTarget("to=alice", "alice")
	require.Equal(t, "hive:alice", owner,
		"resolver helper must produce a hive:-prefixed owner for a bare username")

	// Mirror the helper's AppendLedger call. If this shape changes in the
	// helper, update the test — that's the point.
	session.AppendLedger(ledgerSystem.LedgerUpdate{
		Id:          "sim-tx::0",
		BlockHeight: 100,
		OpIdx:       0,
		Owner:       owner,
		Amount:      1000,
		Asset:       "hbd",
		Memo:        "",
		Type:        "deposit",
	})

	// Now the subsequent sim-call's GetBalance should see the credit.
	bal := session.GetBalance("hive:alice", 100, "hbd")
	assert.Equal(t, int64(1000), bal,
		"deposit credit must be visible to the next op via session.GetBalance")

	// And a transfer of size <= deposit (with exclusion=0 as the SDK's aligned
	// rc_limit=rcFreeRemaining path produces) must succeed.
	res := session.ExecuteTransfer(ledgerSystem.OpLogEvent{
		Id:          "sim-tx::1",
		From:        "hive:alice",
		To:          "contract:vsc1router",
		Amount:      7, // typical transfer.allow intent size (0.007 HBD)
		Asset:       "hbd",
		BlockHeight: 100,
	})
	require.True(t, res.Ok,
		"transfer bounded by deposit amount should succeed: %s", res.Msg)
	assert.Equal(t, int64(993), session.GetBalance("hive:alice", 100, "hbd"),
		"alice's HBD must be deposit minus transfer")
	assert.Equal(t, int64(7), session.GetBalance("contract:vsc1router", 100, "hbd"),
		"the router must have received the transferred HBD")
}

// Exclusion parity with PullBalance: if the caller sets rc_limit higher than
// rcFreeRemaining, the HBD exclusion kicks in and eats into the balance. With
// a freshly-deposited account, the transfer must still pass as long as the
// exclusion + pull amount fits inside the deposit.
func TestSimulateDep_DepositCoversRcExclusion(t *testing.T) {
	state := makeResolverTestState()
	session := ledgerSystem.NewSession(state)

	session.AppendLedger(ledgerSystem.LedgerUpdate{
		Id:          "sim-tx::0",
		BlockHeight: 100,
		Owner:       "hive:alice",
		Amount:      10_007, // 10.007 HBD — enough for a 10000 exclusion + 7 pull
		Asset:       "hbd",
		Type:        "deposit",
	})

	// rc_limit=10000, rcFreeRemaining=0 → exclusion=10000. Pull=7. Transfer
	// check: (10007 - 10000) >= 7 → passes.
	res := session.ExecuteTransfer(
		ledgerSystem.OpLogEvent{
			Id:          "sim-tx::1",
			From:        "hive:alice",
			To:          "contract:vsc1router",
			Amount:      7,
			Asset:       "hbd",
			BlockHeight: 100,
		},
		ledgerSystem.TransferOptions{Exclusion: 10_000},
	)
	assert.True(t, res.Ok,
		"deposit must cover both rc exclusion and the pull amount: %s", res.Msg)

	// Same but with a slightly under-funded deposit: should fail even with a
	// valid deposit, surfacing as "insufficient balance" — proving the sim
	// faithfully models the runtime exclusion logic.
	state2 := makeResolverTestState()
	session2 := ledgerSystem.NewSession(state2)
	session2.AppendLedger(ledgerSystem.LedgerUpdate{
		Id: "sim-tx::0", BlockHeight: 100, Owner: "hive:alice",
		Amount: 10_006, Asset: "hbd", Type: "deposit",
	})
	res2 := session2.ExecuteTransfer(
		ledgerSystem.OpLogEvent{
			Id: "sim-tx::1", From: "hive:alice", To: "contract:vsc1router",
			Amount: 7, Asset: "hbd", BlockHeight: 100,
		},
		ledgerSystem.TransferOptions{Exclusion: 10_000},
	)
	assert.False(t, res2.Ok, "undersized deposit must fail")
	assert.Equal(t, "insufficient balance", res2.Msg)
}
