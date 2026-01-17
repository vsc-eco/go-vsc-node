package wasm_e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

func TestContractTestUtil(t *testing.T) {
	fmt.Println("os.Chdir(projectRoot(t))", projectRoot(t), os.Chdir(projectRoot(t)))
	wkdir := projectRoot(t)
	WASM_PATH, err := Compile(wkdir)
	if err != nil {
		t.Fatalf("Failed to compile: %v", err)
	}
	fmt.Println("WASM file compiled successfully:", WASM_PATH)
	WASM_TEST_CODE, err := os.ReadFile(WASM_PATH) // This is just to ensure the file exists and can be read.
	if err != nil {
		t.Fatalf("Failed to read WASM file: %v", err)
	}

	fmt.Println("WASM_TEST_CODE:", len(WASM_TEST_CODE), WASM_TEST_CODE[:10], "...")

	contractId := "vscmycontract"
	contractId2 := "vscmycontract2"
	txSelf := stateEngine.TxSelf{
		TxId:                 "sometxid",
		BlockId:              "abcdef",
		Index:                69,
		OpIndex:              0,
		Timestamp:            "2025-09-03T00:00:00",
		RequiredAuths:        []string{"hive:someone"},
		RequiredPostingAuths: []string{},
	}

	ct := test_utils.NewContractTest()
	ct.Deposit("hive:someone", 10000, ledgerDb.AssetHive)
	ct.Deposit("hive:someone", 20000, ledgerDb.AssetHbd)
	assert.Equal(t, ct.GetBalance("hive:someone", ledgerDb.AssetHive), int64(10000))
	ct.RegisterContract(contractId, "hive:someone", WASM_TEST_CODE[:])
	ct.RegisterContract(contractId2, "hive:someone", WASM_TEST_CODE[:])
	assert.Equal(t, "", ct.StateGet(contractId, "doesnotexist"))
	ct.StateSet(contractId, "manuallyset", "hi")
	assert.Equal(t, "hi", ct.StateGet(contractId, "manuallyset"))
	ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "drawHive",
		Payload:    json.RawMessage([]byte("1000")),
		RcLimit:    1000,
		Intents: []contracts.Intent{{
			Type: "transfer.allow",
			Args: map[string]string{
				"limit": "1.000",
				"token": "hive",
			},
		}},
	})
	assert.Equal(t, ct.GetBalance("hive:someone", ledgerDb.AssetHive), int64(9000))
	assert.Equal(t, ct.GetBalance("contract:vscmycontract", ledgerDb.AssetHive), int64(1000))

	ledgerErr := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "drawHive",
		Payload:    json.RawMessage([]byte("1000")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.False(t, ledgerErr.Success)
	assert.Equal(t, ledgerErr.Err, contracts.LEDGER_INTENT_ERROR)

	abortResult := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "abortMe",
		Payload:    json.RawMessage([]byte("aborted successfully")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.False(t, abortResult.Success)
	assert.GreaterOrEqual(t, abortResult.RcUsed, int64(100))

	revertResult := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "revertMe",
		Payload:    json.RawMessage([]byte("reverted successfully")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.False(t, revertResult.Success)
	assert.GreaterOrEqual(t, revertResult.RcUsed, int64(100))

	nonExistent := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "doesNotExist",
		Payload:    json.RawMessage([]byte("1000")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.False(t, nonExistent.Success)
	assert.Equal(t, nonExistent.Err, contracts.WASM_FUNC_NOT_FND)

	dumpEnvResult := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "dumpEnv",
		Payload:    json.RawMessage([]byte("")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.GreaterOrEqual(t, len(dumpEnvResult.Logs[contractId].Logs), 1)
	assert.LessOrEqual(t, dumpEnvResult.RcUsed, int64(500))
	assert.True(t, dumpEnvResult.Success)

	dumpEnvKeyResult := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "dumpEnvKey",
		Payload:    json.RawMessage([]byte("contract.id")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.True(t, dumpEnvKeyResult.Success)

	setStr := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "setString",
		Payload:    json.RawMessage([]byte("myString,hello world")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.Equal(t, ct.StateGet(contractId, "myString"), "hello world")
	assert.Equal(t, "hello world", string(setStr.StateDiff[contractId].KeyDiff["myString"].Current))

	ct.StateSet(contractId, "myString2", "changethis")
	assert.Equal(t, ct.StateGet(contractId, "myString2"), "changethis")

	setStr2 := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "setString",
		Payload:    json.RawMessage([]byte("myString2,clearthis")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.Equal(t, "changethis", string(setStr2.StateDiff[contractId].KeyDiff["myString2"].Previous))
	assert.Equal(t, "clearthis", string(setStr2.StateDiff[contractId].KeyDiff["myString2"].Current))

	clearStr := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "clearString",
		Payload:    json.RawMessage([]byte("myString2")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.Equal(t, ct.StateGet(contractId, "myString2"), "")
	assert.True(t, clearStr.StateDiff[contractId].Deletions["myString2"])

	icGetStr := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId2,
		Action:     "contractGetString",
		Payload:    json.RawMessage([]byte(contractId + ",myString")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.True(t, icGetStr.Success)
	assert.Equal(t, "hello world", icGetStr.Ret)

	icCall := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "contractCall",
		Payload:    json.RawMessage([]byte(contractId2 + ",dumpEnv,a")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.True(t, icCall.Success)
	assert.GreaterOrEqual(t, len(icCall.Logs[contractId2].Logs), 1)

	icInfCall := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "infiniteRecursion",
		Payload:    json.RawMessage([]byte("a")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.False(t, icInfCall.Success)
	assert.Equal(t, contracts.IC_RCSE_LIMIT_HIT, icInfCall.Err)

	icRevert := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "contractCall",
		Payload:    json.RawMessage([]byte(contractId2 + ",revertMe,a")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.False(t, icRevert.Success)
	assert.Equal(t, "symbol_here", icRevert.Err)

	failedContractPull := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId2,
		Action:     "contractCall",
		Payload:    json.RawMessage([]byte(contractId + ",drawHive,20")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.False(t, failedContractPull.Success)
	assert.Equal(t, contracts.LEDGER_ERROR, failedContractPull.Err)

	ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "contractCall",
		Payload:    json.RawMessage([]byte(contractId2 + ",drawHive,20")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	assert.Equal(t, ct.GetBalance("contract:vscmycontract", ledgerDb.AssetHive), int64(980))
	assert.Equal(t, ct.GetBalance("contract:vscmycontract2", ledgerDb.AssetHive), int64(20))
}
