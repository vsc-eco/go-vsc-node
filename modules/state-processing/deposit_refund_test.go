package state_engine_test

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/vsc-eco/hivego"
)

// DEPOSIT-REFUND-1: a Hive transaction that deposits to the gateway and whose
// Magi call fails (out of RC, an allowance below the amount, a malformed call)
// sends the deposit back to its sender's Hive account from 0.17.0. Below the
// line it stays on the Magi balance, as it always did.

// drFailedCallTx is one Hive transaction: alice deposits 10 HBD to the gateway
// and calls a contract that does not exist (the call fails).
func drFailedCallTx(te *testEnv, txId string) {
	call, _ := json.Marshal(map[string]interface{}{
		"net_id":      "vsc-mocknet",
		"contract_id": "vsc1NoSuchContract",
		"action":      "execute",
		"payload":     "{}",
		"rc_limit":    1000,
		"intents":     []interface{}{},
	})
	te.Creator.BroadcastOps([]hivego.Operation{
		{
			Type: "transfer",
			Value: map[string]interface{}{
				"from": "alice",
				"to":   "vsc.gateway",
				"amount": map[string]interface{}{
					"amount":    "10",
					"nai":       "@@000000013",
					"precision": 3,
				},
				"memo": "",
			},
		},
		{
			Type: "custom_json",
			Value: map[string]interface{}{
				"id":                     "vsc.call",
				"json":                   string(call),
				"required_auths":         []string{"alice"},
				"required_posting_auths": []string{},
			},
		},
	}, txId)
	te.processAndWait()
	te.Reader.MineNullBlocks(10)
	time.Sleep(300 * time.Millisecond)
}

// drRefund is the refund withdrawal the engine recorded for txId, if any.
func drRefund(te *testEnv, txId string) (found bool, to string, amount int64, inOutput bool) {
	for _, op := range te.SE.LedgerState.Oplog {
		if op.Type == "withdraw" && op.Id == txId+"-refund" {
			found, to, amount = true, op.To, op.Amount
		}
	}
	for _, id := range te.SE.TxOutput[txId].LedgerIds {
		if id == txId+"-refund" {
			inOutput = true
		}
	}
	return
}

func TestDepositRefund_FailedCallSendsTheDepositBackToHive(t *testing.T) {
	te := newTestEnv()
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 17) // 0.17 active
	const txId = "drtx-refunded"
	drFailedCallTx(te, txId)

	if out, ok := te.SE.TxOutput[txId]; !ok || out.Ok {
		t.Fatalf("the call should have failed: output %+v (recorded %v)", out, ok)
	}
	found, to, amount, inOutput := drRefund(te, txId)
	if !found || to != "hive:alice" || amount != 10 || !inOutput {
		t.Fatalf("refund withdrawal found=%v to=%q amount=%d in the tx's ledger ids=%v; want 10 back to hive:alice\noplog: %+v",
			found, to, amount, inOutput, te.SE.LedgerState.Oplog)
	}
}

func TestDepositRefund_BelowTheLineTheDepositStaysOnMagi(t *testing.T) {
	te := newTestEnv()
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 16) // 0.16: before the line
	const txId = "drtx-old"
	drFailedCallTx(te, txId)

	if found, _, _, _ := drRefund(te, txId); found {
		t.Fatal("refunded below 0.17.0: settled history would re-execute differently")
	}
	if bal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd"); bal != 10 {
		t.Fatalf("alice's Magi HBD %d, want the 10 deposited (unchanged rule below the line)", bal)
	}
}

func TestDepositRefund_ADepositAloneOrASucceedingTxIsNotRefunded(t *testing.T) {
	te := newTestEnv()
	te.ElectionDb.ElectionsByHeight[1] = versionedElection(1, 1, 0, 17)
	// A plain deposit (no other op): it is what the sender asked for.
	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.processAndWait()
	// A deposit with a transfer that succeeds, in one transaction.
	transfer, _ := json.Marshal(map[string]interface{}{
		"from": "hive:alice", "to": "hive:bob", "amount": "0.005", "asset": "hbd", "memo": "", "net_id": "vsc-mocknet",
	})
	te.Creator.BroadcastOps([]hivego.Operation{
		{Type: "transfer", Value: map[string]interface{}{
			"from": "alice", "to": "vsc.gateway", "memo": "",
			"amount": map[string]interface{}{"amount": "10", "nai": "@@000000013", "precision": 3},
		}},
		{Type: "custom_json", Value: map[string]interface{}{
			"id": "vsc.transfer", "json": string(transfer), "required_auths": []string{"alice"}, "required_posting_auths": []string{},
		}},
	}, "drtx-ok")
	te.processAndWait()
	te.Reader.MineNullBlocks(10)
	time.Sleep(300 * time.Millisecond)

	for _, op := range te.SE.LedgerState.Oplog {
		if op.Type == "withdraw" {
			t.Fatalf("a withdrawal %+v for a deposit whose transaction did not fail", op)
		}
	}
	if out := te.SE.TxOutput["drtx-ok"]; !out.Ok {
		t.Fatalf("the deposit + transfer transaction should have succeeded: %+v", out)
	}
}
