//go:build evm_realwasm

package main

import (
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"
)

// rwCallPosting runs action as a Hive account signing with its posting key
// only (required_posting_auths), as the node builds it for such a custom_json.
func rwCallPosting(ct *test_utils.ContractTest, caller, action string) test_utils.ContractTestCallResult {
	return ct.Call(stateEngine.TxVscCallContract{
		Self: stateEngine.TxSelf{
			TxId: "rwtx", BlockId: "block:rw", Index: 1, Timestamp: "2026-10-18T00:00:00",
			RequiredAuths: []string{}, RequiredPostingAuths: []string{caller},
		},
		ContractId: rwBridge, Action: action, Payload: []byte(`{}`),
		RcLimit: 10000, Intents: []contracts.Intent{}, Caller: caller,
	})
}

// 18p on the bridge wasm (BTC contract 62ba2d9): the four rotation exports need
// the owner's or a hive: operator's active key; a posting key alone is refused.
// A did:pkh operator (the bot) has no posting variant and still passes the gate.
// Needs a bridge wasm with the account-mapping checkOperator fix
// (fix/evm-operator-active-auth-1109); on develop 51e32c4 it fails, which is the
// gap: the owner's posting key runs migrateVault.
func TestRealWasm_P18p_OperatorExportsNeedActiveAuth(t *testing.T) {
	const activeRefusal = "requires active authority"
	const notAllowed = "action must be performed by the contract owner or the vault operator"
	gated := func(r test_utils.ContractTestCallResult) bool {
		return strings.Contains(r.ErrMsg, activeRefusal) || strings.Contains(r.ErrMsg, notAllowed)
	}
	for _, action := range []string{"migrateVault", "redriveSweep", "retireVault", "writeOffDust"} {
		for _, op := range []string{"hive:rotation-op", "did:pkh:eip155:1:0x9861820c2E8399AEbf96D4e23A30DEA4AF2d532D"} {
			ct := rwContract(t)
			rwVaultKey(t, ct)
			if r := rwCall(ct, "hive:owner", "setVaultOperator", []byte(op)); !r.Success {
				t.Fatalf("setVaultOperator %s: %s: %s", op, r.Err, r.ErrMsg)
			}
			if r := rwCallPosting(ct, "hive:owner", action); r.Success || !strings.Contains(r.ErrMsg, activeRefusal) {
				t.Errorf("%s by the owner's posting key: success=%v msg=%q, want the active-authority refusal", action, r.Success, r.ErrMsg)
			}
			if r := rwCall(ct, "hive:owner", action, []byte(`{}`)); gated(r) {
				t.Errorf("%s by the owner's active key refused at the gate: %q", action, r.ErrMsg)
			}
			if strings.HasPrefix(op, "hive:") {
				if r := rwCallPosting(ct, op, action); r.Success || !strings.Contains(r.ErrMsg, activeRefusal) {
					t.Errorf("%s by a hive: operator's posting key: success=%v msg=%q, want the active-authority refusal", action, r.Success, r.ErrMsg)
				}
			}
			// The operator as the node presents it for its own tx (an L2 tx from a
			// did:pkh lists the did in required_auths; a hive: one signs active).
			if r := rwCall(ct, op, action, []byte(`{}`)); gated(r) {
				t.Errorf("%s by operator %s refused at the gate: %q", action, op, r.ErrMsg)
			}
			if r := rwCall(ct, "hive:stranger", action, []byte(`{}`)); r.Success || !strings.Contains(r.ErrMsg, notAllowed) {
				t.Errorf("%s by a stranger: success=%v msg=%q, want refused", action, r.Success, r.ErrMsg)
			}
		}
	}
}
