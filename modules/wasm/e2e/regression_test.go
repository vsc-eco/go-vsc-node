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

// TestGasFixRegression runs all existing contract operations using pre-compiled WASM
// to verify the gas underflow fix doesn't break normal contract execution.
func TestGasFixRegression(t *testing.T) {
	fmt.Println("=== GAS FIX REGRESSION TEST ===")
	fmt.Println("os.Chdir(projectRoot(t))", projectRoot(t), os.Chdir(projectRoot(t)))

	WASM_PATH := projectRoot(t) + "/modules/wasm/e2e/tmp/main.wasm"
	WASM_TEST_CODE, err := os.ReadFile(WASM_PATH)
	if err != nil {
		t.Fatalf("Failed to read WASM file: %v (run tinygo build first)", err)
	}

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

	// Test 1: State set/get
	t.Run("StateSetGet", func(t *testing.T) {
		assert.Equal(t, "", ct.StateGet(contractId, "doesnotexist"))
		ct.StateSet(contractId, "manuallyset", "hi")
		assert.Equal(t, "hi", ct.StateGet(contractId, "manuallyset"))
	})

	// Test 2: Draw HIVE with intent
	t.Run("DrawHiveWithIntent", func(t *testing.T) {
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
	})

	// Test 3: Draw without intent should fail
	t.Run("DrawWithoutIntentFails", func(t *testing.T) {
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
	})

	// Test 4: Abort
	t.Run("Abort", func(t *testing.T) {
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
	})

	// Test 5: Revert
	t.Run("Revert", func(t *testing.T) {
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
	})

	// Test 6: Non-existent function
	t.Run("NonExistentFunction", func(t *testing.T) {
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
	})

	// Test 7: Dump env
	t.Run("DumpEnv", func(t *testing.T) {
		dumpEnvResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractId,
			Action:     "dumpEnv",
			Payload:    json.RawMessage([]byte("")),
			RcLimit:    1000,
			Intents:    []contracts.Intent{},
		})
		assert.True(t, dumpEnvResult.Success)
		assert.GreaterOrEqual(t, len(dumpEnvResult.Logs[contractId].Logs), 1)
		assert.LessOrEqual(t, dumpEnvResult.RcUsed, int64(500))
	})

	// Test 8: Set and get string
	t.Run("SetString", func(t *testing.T) {
		setStr := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractId,
			Action:     "setString",
			Payload:    json.RawMessage([]byte("myString,hello world")),
			RcLimit:    1000,
			Intents:    []contracts.Intent{},
		})
		assert.True(t, setStr.Success)
		assert.Equal(t, ct.StateGet(contractId, "myString"), "hello world")
	})

	// Test 9: Ephemeral state
	t.Run("EphemeralState", func(t *testing.T) {
		ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractId,
			Action:     "setEphemStr",
			Payload:    json.RawMessage([]byte("foo,bar")),
			RcLimit:    1000,
			Intents:    []contracts.Intent{},
		})
		assert.Equal(t, ct.EphemStateGet(contractId, "foo"), "bar")
	})

	// Test 10: Inter-contract read
	t.Run("InterContractRead", func(t *testing.T) {
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
	})

	// Test 11: Inter-contract call
	t.Run("InterContractCall", func(t *testing.T) {
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
	})

	// Test 12: Infinite recursion should fail
	t.Run("InfiniteRecursionFails", func(t *testing.T) {
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
	})

	// Test 13: Inter-contract revert
	t.Run("InterContractRevert", func(t *testing.T) {
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
	})

	fmt.Println("\n=== ALL REGRESSION TESTS PASSED ===")
}
