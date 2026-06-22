package state_engine_test

import (
	"testing"

	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	ledgerSystem "vsc-node/modules/ledger-system"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// electionAtVersion returns an ElectionResult pinned to consensus major.consensus.
func electionAtVersion(major, consensus uint64) elections.ElectionResult {
	er := elections.ElectionResult{}
	er.Epoch = 1
	er.VersionMajor = major
	er.ProtocolVersion = consensus
	return er
}

// runStakeMode executes a consensus_stake tx against te.SE with a fresh funded
// ledger session and returns the result. `funded` is credited with HIVE so the
// balance check is never the reason a stake is rejected.
func runStakeMode(te *testEnv, from, to, funded string) stateEngine.TxResult {
	balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		funded: {{Account: funded, BlockHeight: 0, Hive: 100000}},
	})
	ls := ledgerSystem.New(balDb, newMockLedgerDb(), nil, newMockActionsDb(), nil)
	session := ls.NewEmptySession(ls.NewEmptyState(), 1)
	tx := &stateEngine.TxConsensusStake{
		Self: stateEngine.TxSelf{
			TxId:          "mode-test",
			OpIndex:       0,
			BlockHeight:   1,
			RequiredAuths: []string{from},
		},
		From:   from,
		To:     to,
		Amount: "10.000",
		Asset:  "hive", // explicit so the parse never masks the mode gate under test
		NetId:  "vsc-mocknet",
	}
	return tx.ExecuteTx(te.SE, session, nil, nil, "")
}

// Consensus 0.3.0 opt-in: a node must accept delegations before a third party
// can stake to it. Deactivated (the default) rejects; Share and Custom accept;
// the operator's own self-stake is always allowed; pre-0.3.0 the gate is inert.
func TestConsensusStakeDelegationModeGate(t *testing.T) {
	const alice = "hive:alice"
	const opB = "hive:operatorb"

	setWitnessMode := func(te *testEnv, mode string) {
		te.WitnessDb.ByAccount["operatorb"] = &witnesses.Witness{
			Account:        "operatorb",
			DelegationMode: mode,
		}
	}

	t.Run("deactivated default (no announcement) rejects third-party", func(t *testing.T) {
		te := newTestEnv()
		te.ElectionDb.ElectionsByHeight[1] = electionAtVersion(0, 3)
		// no witness record for operatorb → default Deactivated
		res := runStakeMode(te, alice, opB, alice)
		assert.False(t, res.Success, "third-party delegation to a Deactivated node must be rejected")
		assert.Equal(t, "node does not accept delegations", res.Ret)
	})

	t.Run("explicit deactivated rejects third-party", func(t *testing.T) {
		te := newTestEnv()
		te.ElectionDb.ElectionsByHeight[1] = electionAtVersion(0, 3)
		setWitnessMode(te, "deactivated")
		res := runStakeMode(te, alice, opB, alice)
		assert.False(t, res.Success)
		assert.Equal(t, "node does not accept delegations", res.Ret)
	})

	t.Run("share accepts third-party", func(t *testing.T) {
		te := newTestEnv()
		te.ElectionDb.ElectionsByHeight[1] = electionAtVersion(0, 3)
		setWitnessMode(te, "share")
		res := runStakeMode(te, alice, opB, alice)
		assert.True(t, res.Success, "share-mode node must accept delegation: %s", res.Ret)
	})

	t.Run("custom accepts third-party", func(t *testing.T) {
		te := newTestEnv()
		te.ElectionDb.ElectionsByHeight[1] = electionAtVersion(0, 3)
		setWitnessMode(te, "custom")
		res := runStakeMode(te, alice, opB, alice)
		assert.True(t, res.Success, "custom-mode node must accept delegation: %s", res.Ret)
	})

	t.Run("operator self-stake always allowed even when deactivated", func(t *testing.T) {
		te := newTestEnv()
		te.ElectionDb.ElectionsByHeight[1] = electionAtVersion(0, 3)
		setWitnessMode(te, "deactivated")
		res := runStakeMode(te, opB, opB, opB) // from == to
		assert.True(t, res.Success, "operator self-stake must never be gated: %s", res.Ret)
	})

	t.Run("pre-0.3.0 gate inert: third-party to deactivated still allowed at 0.2.0 (legacy)", func(t *testing.T) {
		te := newTestEnv()
		// Even at the already-shipped 0.2.0 floor, delegation is inert until 0.3.0,
		// so a third-party stake follows the legacy (ungated) path.
		te.ElectionDb.ElectionsByHeight[1] = electionAtVersion(0, 2)
		res := runStakeMode(te, alice, opB, alice)
		assert.True(t, res.Success, "pre-0.3.0 stake must follow the legacy (ungated) path: %s", res.Ret)
	})
}
