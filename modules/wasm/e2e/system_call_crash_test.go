package wasm_e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
)

// TestSystemCallCrash demonstrates that system.call panics when the target
// function has a different arity than the hard-coded type assertion on line 209
// of modules/wasm/sdk/sdk.go:
//
//	f.(func(context.Context, any) SdkResult)(ctx, valArg.Arg0)
//
// Any function with more or fewer value parameters (e.g. hive.transfer which
// takes 3 value args) causes an interface conversion panic that crashes the
// host process — a malicious contract can take down any node that executes it.
func TestSystemCallCrash(t *testing.T) {
	fmt.Println("=== SYSTEM.CALL TYPE-ASSERTION CRASH TEST ===")
	fmt.Println("os.Chdir(projectRoot(t))", projectRoot(t), os.Chdir(projectRoot(t)))

	WASM_PATH := projectRoot(t) + "/modules/wasm/e2e/tmp/main.wasm"
	WASM_TEST_CODE, err := os.ReadFile(WASM_PATH)
	if err != nil {
		t.Fatalf("Failed to read WASM file: %v (run tinygo build first)", err)
	}

	contractId := "vsc-crash-test"
	txSelf := stateEngine.TxSelf{
		TxId:                 "crash-tx-001",
		BlockId:              "abcdef",
		Index:                0,
		OpIndex:              0,
		Timestamp:            "2026-03-28T00:00:00",
		RequiredAuths:        []string{"hive:attacker"},
		RequiredPostingAuths: []string{},
	}

	ct := test_utils.NewContractTest()
	ct.RegisterContract(contractId, "hive:attacker", WASM_TEST_CODE[:])

	// The bug: system.call looks up the target function (e.g. hive.transfer)
	// in SdkNamespaces and does a direct type assertion to
	// func(context.Context, any) SdkResult — but hive.transfer has signature
	// func(context.Context, any, any, any) SdkResult. The assertion panics.
	//
	// If the node doesn't recover from this panic, the process crashes.
	// We wrap the call in a deferred recover to detect this.

	fmt.Println("\n--- Calling systemCallCrash ---")
	fmt.Println("Target: system.call(\"hive.transfer\", ...)")
	fmt.Println("Expected: panic due to type assertion mismatch")

	panicked := false
	var panicValue interface{}
	var result test_utils.ContractTestCallResult

	func() {
		defer func() {
			if r := recover(); r != nil {
				panicked = true
				panicValue = r
			}
		}()
		result = ct.Call(stateEngine.TxVscCallContract{
			Self:       txSelf,
			ContractId: contractId,
			Action:     "systemCallCrash",
			Payload:    json.RawMessage([]byte("")),
			RcLimit:    1000,
			Intents:    []contracts.Intent{},
		})
	}()

	fmt.Println("\n--- RESULTS ---")
	if panicked {
		fmt.Println("NODE CRASHED: system.call caused an unrecovered panic")
		fmt.Printf("Panic value: %v\n", panicValue)
		fmt.Println("\n!!! EXPLOIT CONFIRMED !!!")
		fmt.Println("A malicious contract can crash any node by calling system.call")
		fmt.Println("with a function whose arity doesn't match the hard-coded assertion.")
		fmt.Println()
		fmt.Println("Root cause: modules/wasm/sdk/sdk.go line 209")
		fmt.Println("  f.(func(context.Context, any) SdkResult)(ctx, valArg.Arg0)")
		fmt.Println()
		fmt.Println("Fix: use reflect.ValueOf(f).Call(...) like executeImport does,")
		fmt.Println("or add a type-switch for each supported arity.")

		// We mark this as a test failure because the node should NOT crash
		t.Fatalf("CRITICAL: system.call panics the host process — node crash exploit confirmed. Panic: %v", panicValue)
	} else {
		fmt.Printf("Success:  %v\n", result.Success)
		fmt.Printf("Return:   %s\n", result.Ret)
		fmt.Printf("Error:    %s\n", result.Err)
		fmt.Printf("ErrMsg:   %s\n", result.ErrMsg)

		if result.Success {
			fmt.Println("\nNo panic and call succeeded — bug may be fixed or not triggered.")
			fmt.Printf("Return value: %s\n", result.Ret)
		} else {
			fmt.Println("\nNo panic, call returned an error — this is the CORRECT behavior.")
			fmt.Println("system.call should return an error, not crash the process.")
			assert.False(t, result.Success, "system.call with mismatched arity should fail gracefully")
		}
	}

	fmt.Println("\n=== TEST COMPLETE ===")
}
