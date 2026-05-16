package state_engine_test

import (
	"testing"

	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// review2 MEDIUM #45 — TxConsensusStake/Unstake parse an `asset` field
// from the signed custom_json payload but ExecuteTx never validated it;
// the ledger unconditionally moves "hive". A consensus_stake claiming
// asset "hbd"/"hbd_savings"/etc was therefore silently treated as a HIVE
// stake (confused-field: the signed intent and executed effect disagree).
//
// Tested at ExecuteTx level — the mock block harness does not settle
// consensus oplog, so the differential is observed on the TxResult.
//
// Differential: on the #170 baseline the bogus-asset stake passes the
// (missing) asset check and the ledger stakes HIVE → Success:true (RED).
// On fix/review2 it is rejected with "Invalid asset" before the ledger is
// touched → Success:false (GREEN). An asset-omitted stake still succeeds
// on both arms (sanity — guard not too strict).
func TestReview2ConsensusStakeAssetValidated(t *testing.T) {
	te := newTestEnv() // wired StateEngine; MocknetConfig NetId = vsc-mocknet

	newSession := func() ledgerSystem.LedgerSession {
		// Hive is fixed-point ×1000 (SafeParseHiveFloat); 100000 = 100.000.
		balDb := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
			"hive:alice": {{Account: "hive:alice", BlockHeight: 0, Hive: 100000}},
		})
		ls := ledgerSystem.New(balDb, newMockLedgerDb(), nil, newMockActionsDb())
		return ls.NewEmptySession(ls.NewEmptyState(), 1)
	}

	self := stateEngine.TxSelf{
		TxId:          "rev2-45",
		OpIndex:       0,
		BlockHeight:   1,
		RequiredAuths: []string{"hive:alice"},
	}

	// Bogus asset — baseline ignores it and stakes HIVE; fix rejects.
	badStake := &stateEngine.TxConsensusStake{
		Self:   self,
		From:   "hive:alice",
		To:     "hive:alice",
		Amount: "10.000",
		Asset:  "hbd",
		NetId:  "vsc-mocknet",
	}
	res := badStake.ExecuteTx(te.SE, newSession(), nil, nil, "")
	assert.False(t, res.Success,
		"review2 #45: asset='hbd' stake must be rejected (baseline stakes HIVE: Success=true)")
	assert.Equal(t, "Invalid asset", res.Ret,
		"review2 #45: expected explicit asset rejection")

	// Same for consensus_unstake.
	badUnstake := &stateEngine.TxConsensusUnstake{
		Self:   self,
		From:   "hive:alice",
		To:     "hive:alice",
		Amount: "10.000",
		Asset:  "hbd_savings",
		NetId:  "vsc-mocknet",
	}
	resU := badUnstake.ExecuteTx(te.SE, newSession(), nil, nil, "")
	assert.False(t, resU.Success, "review2 #45: asset='hbd_savings' unstake must be rejected")
	assert.Equal(t, "Invalid asset", resU.Ret,
		"review2 #45: baseline reaches ledger and returns 'insufficient balance' instead")

	// Sanity: asset-omitted stake (the only shape the crafter emits) still
	// reaches the ledger and succeeds — identical on both arms.
	okStake := &stateEngine.TxConsensusStake{
		Self:   self,
		From:   "hive:alice",
		To:     "hive:alice",
		Amount: "10.000",
		NetId:  "vsc-mocknet",
	}
	resOk := okStake.ExecuteTx(te.SE, newSession(), nil, nil, "")
	assert.True(t, resOk.Success,
		"review2 #45: valid (asset-omitted) stake must still succeed (guard not too strict)")

	// Explicit asset:"hive" is also accepted.
	okStakeHive := &stateEngine.TxConsensusStake{
		Self:   self,
		From:   "hive:alice",
		To:     "hive:alice",
		Amount: "10.000",
		Asset:  "hive",
		NetId:  "vsc-mocknet",
	}
	resOkHive := okStakeHive.ExecuteTx(te.SE, newSession(), nil, nil, "")
	assert.True(t, resOkHive.Success,
		"review2 #45: explicit asset='hive' must still succeed")
}
