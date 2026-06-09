package wasm_e2e

// review8 DX-H6 enabler — try/catch inter-contract calls.
//
// Today a sub-call that reverts TRAPS the caller (the wasmedge host shim returns
// Result_Fail) and there is no per-sub-call state isolation, so a caller cannot
// recover from a failed callee. This proves the new opt-in try/catch primitive:
//   - a reverting callee no longer traps the caller; the caller gets a structured
//     {"ok":false,...} outcome and keeps running;
//   - every state write the callee made before reverting is rolled back;
//   - a successful callee still commits normally.

import (
	"encoding/json"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type tryOutcome struct {
	Ok        bool   `json:"ok"`
	Result    string `json:"result"`
	ErrorCode string `json:"error_code"`
	Error     string `json:"error"`
}

func TestReview8_TryContractCall(t *testing.T) {
	wkdir := projectRoot(t)
	require.NoError(t, os.Chdir(wkdir))
	wasmPath, err := Compile(wkdir)
	require.NoError(t, err, "compile test wasm")
	code, err := os.ReadFile(wasmPath)
	require.NoError(t, err)

	caller := "vsccaller"
	callee := "vsccallee"
	self := stateEngine.TxSelf{
		TxId: "trycall-tx", BlockId: "blk", Index: 0, OpIndex: 0,
		Timestamp: "2025-09-03T00:00:00", RequiredAuths: []string{"hive:someone"}, RequiredPostingAuths: []string{},
	}

	ct := test_utils.NewContractTest()
	ct.RegisterContract(caller, "hive:someone", code[:])
	ct.RegisterContract(callee, "hive:someone", code[:])

	// --- Case 1: callee reverts after writing state -> caught, rolled back ---
	// tryThenSet payload: calleeId;method;callPayload;markerKey;markerVal
	r := ct.Call(stateEngine.TxVscCallContract{
		Self:       self,
		ContractId: caller,
		Action:     "tryThenSet",
		Payload:    json.RawMessage([]byte(callee + ";setThenAbort;doomed,written;caller_marker;continued")),
		RcLimit:    2000,
		Intents:    []contracts.Intent{},
	})
	require.True(t, r.Success, "caller must NOT trap when the callee reverts in try mode: %s %s", r.Err, r.ErrMsg)

	var out tryOutcome
	require.NoError(t, json.Unmarshal([]byte(r.Ret), &out), "outcome JSON: %q", r.Ret)
	assert.False(t, out.Ok, "outcome must report the sub-call failed")
	assert.NotEmpty(t, out.Error, "outcome must carry the callee's error")

	// The callee's pre-abort write is rolled back...
	assert.Equal(t, "", ct.StateGet(callee, "doomed"),
		"callee state write must be rolled back when the try-call fails")
	// ...and the caller kept running and committed its own write.
	assert.Equal(t, "continued", ct.StateGet(caller, "caller_marker"),
		"caller must keep running after a caught failure")

	// --- Case 2: callee succeeds -> committed normally through a try-call ---
	r2 := ct.Call(stateEngine.TxVscCallContract{
		Self:       self,
		ContractId: caller,
		Action:     "tryThenSet",
		Payload:    json.RawMessage([]byte(callee + ";setString;kept,value;caller_marker2;done2")),
		RcLimit:    2000,
		Intents:    []contracts.Intent{},
	})
	require.True(t, r2.Success, "successful try-call must succeed: %s %s", r2.Err, r2.ErrMsg)

	var out2 tryOutcome
	require.NoError(t, json.Unmarshal([]byte(r2.Ret), &out2), "outcome JSON: %q", r2.Ret)
	assert.True(t, out2.Ok, "outcome must report the sub-call succeeded")

	assert.Equal(t, "value", ct.StateGet(callee, "kept"),
		"a successful callee's write must persist through a try-call")
	assert.Equal(t, "done2", ct.StateGet(caller, "caller_marker2"))
}
