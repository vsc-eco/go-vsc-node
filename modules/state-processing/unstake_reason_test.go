package state_engine_test

import (
	"encoding/json"
	"testing"
	"time"

	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/require"
)

// UNSTAKE-REASON-1: a refused op used to leave only status FAILED, so an operator whose
// consensus_unstake was refused could not see why. The refusal text is now kept on the
// transaction record that findTransaction returns.

func (te *testEnv) settleSlot() {
	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)
}

func TestUnstakeReason_RefusedConsensusUnstakeKeepsItsReason(t *testing.T) {
	te := newTestEnv()
	te.Creator.Transfer("alice", "vsc.gateway", "10", "HIVE", "")
	te.processAndWait()

	unstake, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "hive:alice",
		"amount": "1.000",
		"asset":  "hive",
		"net_id": "vsc-mocknet",
	})
	conf := te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"alice"},
		Id:            "vsc.consensus_unstake",
		Json:          string(unstake),
	})
	te.processAndWait()
	te.settleSlot()

	rec, ok := te.TxDb.Records[conf.Id]
	require.True(t, ok, "the unstake was not indexed")
	require.NotEmpty(t, rec.Error, "a refused consensus_unstake must keep its refusal text")
	t.Logf("consensus_unstake refused: %q", rec.Error)
}

func TestUnstakeReason_FailedTransferKeepsItsReasonAndASuccessHasNone(t *testing.T) {
	te := newTestEnv()
	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.processAndWait()

	transfer := func(amount string) string {
		j, _ := json.Marshal(map[string]interface{}{
			"from":   "hive:alice",
			"to":     "hive:bob",
			"amount": amount,
			"asset":  "hbd",
			"memo":   "",
			"net_id": "vsc-mocknet",
		})
		return te.Creator.CustomJson(stateEngine.MockJson{
			RequiredAuths: []string{"alice"},
			Id:            "vsc.transfer",
			Json:          string(j),
		}).Id
	}
	failed := transfer("50.000")
	te.processAndWait()
	te.settleSlot()
	passed := transfer("0.005") // 5 of the 10 base units deposited
	te.processAndWait()
	te.settleSlot()

	require.NotEmpty(t, te.TxDb.Records[failed].Error, "a transfer above the balance must say why it failed")
	require.NotContains(t, te.TxDb.Records[failed].Error, "RequiredAuths", "it must fail on the balance, not the auth")
	require.Empty(t, te.TxDb.Records[passed].Error, "a transfer that went through carries no error")
	// This env never folds a slot's ledger into balances, so read the batch outcome.
	require.True(t, te.SE.TxOutput[passed].Ok, "the small transfer must have executed and succeeded")
	require.False(t, te.SE.TxOutput[failed].Ok)
	t.Logf("transfer refused: %q", te.TxDb.Records[failed].Error)
}
