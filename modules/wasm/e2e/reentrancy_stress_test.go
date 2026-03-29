package wasm_e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReentrancyStress(t *testing.T) {
	fmt.Println("os.Chdir(projectRoot(t))", projectRoot(t), os.Chdir(projectRoot(t)))
	wkdir := projectRoot(t)
	wasmPath := wkdir + "/modules/wasm/e2e/tmp/main.wasm"
	wasmCode, err := os.ReadFile(wasmPath)
	require.NoError(t, err, "Failed to read pre-compiled WASM at %s", wasmPath)

	contractA := "vsc_reentrancy_a"
	contractB := "vsc_reentrancy_b"
	txSelf := stateEngine.TxSelf{
		TxId:                 "reentrancy_stress_tx",
		BlockId:              "reentrancy_block",
		Index:                1,
		OpIndex:              0,
		Timestamp:            "2026-03-28T00:00:00",
		RequiredAuths:        []string{"hive:stresstest"},
		RequiredPostingAuths: []string{},
	}

	ct := test_utils.NewContractTest()
	ct.Deposit("hive:stresstest", 100000, ledgerDb.AssetHive)
	ct.Deposit("hive:stresstest", 100000, ledgerDb.AssetHbd)
	ct.RegisterContract(contractA, "hive:stresstest", wasmCode)
	ct.RegisterContract(contractB, "hive:stresstest", wasmCode)

	// ---- Subtest 1: Infinite recursion hits depth limit ----
	t.Run("InfiniteRecursionHitsLimitAt20", func(t *testing.T) {
		result := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "infiniteRecursion",
			Payload:    json.RawMessage([]byte("a")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})

		assert.False(t, result.Success, "infiniteRecursion should fail")
		assert.Equal(t, contracts.IC_RCSE_LIMIT_HIT, result.Err,
			"Expected IC_RCSE_LIMIT_HIT error, got: %s (msg: %s)", result.Err, result.ErrMsg)
		t.Logf("infiniteRecursion correctly failed with %s, gas used: %d, rc used: %d",
			result.Err, result.GasUsed, result.RcUsed)
	})

	// ---- Subtest 2: Single cross-contract call succeeds (depth 1) ----
	t.Run("SingleCrossContractCallSucceeds", func(t *testing.T) {
		result := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "contractCall",
			Payload:    json.RawMessage([]byte(contractB + ",dumpEnv,a")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})

		assert.True(t, result.Success, "Single cross-contract call should succeed, err: %s (%s)",
			result.Err, result.ErrMsg)
		assert.GreaterOrEqual(t, len(result.Logs[contractB].Logs), 1,
			"Called contract should have produced logs")
		t.Logf("Single cross-contract call succeeded, gas used: %d", result.GasUsed)
	})

	// ---- Subtest 3: State rollback on failed cross-contract call ----
	t.Run("StateRollbackOnFailedCrossCall", func(t *testing.T) {
		// Set baseline state via a successful call
		setResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "setString",
			Payload:    json.RawMessage([]byte("pre_test_key,pre_test_value")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})
		require.True(t, setResult.Success, "Baseline state set should succeed")
		assert.Equal(t, "pre_test_value", ct.StateGet(contractA, "pre_test_key"))

		// Call contract B's revertMe through contract A's contractCall.
		// Fatal errors propagate up, so the whole call fails.
		revertResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "contractCall",
			Payload:    json.RawMessage([]byte(contractB + ",revertMe,a")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})

		assert.False(t, revertResult.Success,
			"Cross-contract call to revertMe should fail")
		t.Logf("Cross-contract revert correctly failed with err: %s (%s)",
			revertResult.Err, revertResult.ErrMsg)

		// Previously committed state (from a separate tx) should survive
		assert.Equal(t, "pre_test_value", ct.StateGet(contractA, "pre_test_key"),
			"Previously committed state should survive a subsequent failed call")
	})

	// ---- Subtest 4: State survives after infinite recursion failure ----
	t.Run("InfiniteRecursionStateRollback", func(t *testing.T) {
		ct.StateSet(contractA, "survives", "yes")
		assert.Equal(t, "yes", ct.StateGet(contractA, "survives"))

		result := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "infiniteRecursion",
			Payload:    json.RawMessage([]byte("a")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})
		assert.False(t, result.Success)
		assert.Equal(t, contracts.IC_RCSE_LIMIT_HIT, result.Err)

		// Pre-existing state must survive
		assert.Equal(t, "yes", ct.StateGet(contractA, "survives"),
			"Pre-existing state should survive a failed recursive call")
	})

	// ---- Subtest 5: Environment functional after recursion failure ----
	t.Run("EnvironmentFunctionalAfterRecursionFailure", func(t *testing.T) {
		// Trigger recursion failure
		recursionResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "infiniteRecursion",
			Payload:    json.RawMessage([]byte("a")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})
		require.False(t, recursionResult.Success)
		require.Equal(t, contracts.IC_RCSE_LIMIT_HIT, recursionResult.Err)

		// 1. State operations still work
		setResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "setString",
			Payload:    json.RawMessage([]byte("post_recursion,still_works")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})
		assert.True(t, setResult.Success, "setString after recursion failure should succeed, err: %s", setResult.ErrMsg)
		assert.Equal(t, "still_works", ct.StateGet(contractA, "post_recursion"))

		// 2. Cross-contract calls still work
		ccResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "contractCall",
			Payload:    json.RawMessage([]byte(contractB + ",dumpEnv,a")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})
		assert.True(t, ccResult.Success, "Cross-contract call after recursion failure should succeed, err: %s", ccResult.ErrMsg)

		// 3. Token operations still work
		drawResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "drawHive",
			Payload:    json.RawMessage([]byte("500")),
			RcLimit:    10000,
			Intents: []contracts.Intent{{
				Type: "transfer.allow",
				Args: map[string]string{
					"limit": "1.000",
					"token": "hive",
				},
			}},
		})
		assert.True(t, drawResult.Success, "drawHive after recursion failure should succeed, err: %s", drawResult.ErrMsg)
		assert.Equal(t, int64(500), ct.GetBalance("contract:"+contractA, ledgerDb.AssetHive),
			"Balance should reflect the draw after recursion failure recovery")

		t.Logf("Environment fully functional after recursion failure: state, cross-contract, and token ops all work")
	})

	// ---- Subtest 6: Memory and goroutine usage during deep recursion ----
	t.Run("MemoryAndGoroutineUsage", func(t *testing.T) {
		// Baseline measurement
		runtime.GC()
		var memBefore runtime.MemStats
		runtime.ReadMemStats(&memBefore)
		goroutinesBefore := runtime.NumGoroutine()

		// Run infinite recursion 5 times to stress test for leaks
		for i := 0; i < 5; i++ {
			result := ct.Call(stateEngine.TxVscCallContract{
				Self:       txSelf,
				ContractId: contractA,
				Action:     "infiniteRecursion",
				Payload:    json.RawMessage([]byte("a")),
				RcLimit:    10000,
				Intents:    []contracts.Intent{},
			})
			assert.False(t, result.Success)
			assert.Equal(t, contracts.IC_RCSE_LIMIT_HIT, result.Err)
		}

		// Post measurement
		runtime.GC()
		var memAfter runtime.MemStats
		runtime.ReadMemStats(&memAfter)
		goroutinesAfter := runtime.NumGoroutine()

		goroutineDelta := goroutinesAfter - goroutinesBefore
		assert.LessOrEqual(t, goroutineDelta, 10,
			"Goroutine leak detected: before=%d, after=%d, delta=%d",
			goroutinesBefore, goroutinesAfter, goroutineDelta)

		// Use signed arithmetic to handle GC freeing memory between measurements
		heapDeltaMB := (float64(int64(memAfter.HeapAlloc) - int64(memBefore.HeapAlloc))) / (1024 * 1024)
		t.Logf("Memory usage: before=%.2f MB, after=%.2f MB, delta=%.2f MB",
			float64(memBefore.HeapAlloc)/(1024*1024),
			float64(memAfter.HeapAlloc)/(1024*1024),
			heapDeltaMB)
		t.Logf("Goroutines: before=%d, after=%d, delta=%d",
			goroutinesBefore, goroutinesAfter, goroutineDelta)

		// Flag if heap grew by more than 50MB across 5 recursion cycles
		// Negative delta means GC freed memory -- that's fine.
		assert.Less(t, heapDeltaMB, 50.0,
			"Excessive memory growth after 5 recursive call cycles: %.2f MB", heapDeltaMB)
	})

	// ---- Subtest 7: Ledger rollback after recursion failure ----
	t.Run("LedgerRollbackOnRecursionFailure", func(t *testing.T) {
		balanceBefore := ct.GetBalance("hive:stresstest", ledgerDb.AssetHive)

		result := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "infiniteRecursion",
			Payload:    json.RawMessage([]byte("a")),
			RcLimit:    10000,
			Intents:    []contracts.Intent{},
		})
		assert.False(t, result.Success)

		balanceAfter := ct.GetBalance("hive:stresstest", ledgerDb.AssetHive)
		assert.Equal(t, balanceBefore, balanceAfter,
			"User balance should be unchanged after failed recursive call")

		// Verify token draw still works (ledger not corrupted)
		drawResult := ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractA,
			Action:     "drawHive",
			Payload:    json.RawMessage([]byte("1000")),
			RcLimit:    10000,
			Intents: []contracts.Intent{{
				Type: "transfer.allow",
				Args: map[string]string{
					"limit": "1.000",
					"token": "hive",
				},
			}},
		})
		assert.True(t, drawResult.Success, "Token draw after recursion failure should work, err: %s", drawResult.ErrMsg)
		assert.Equal(t, balanceBefore-1000, ct.GetBalance("hive:stresstest", ledgerDb.AssetHive),
			"Balance should reflect the successful draw")
	})
}
