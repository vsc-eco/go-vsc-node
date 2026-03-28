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

func TestGasUnderflow(t *testing.T) {
	fmt.Println("=== GAS UNDERFLOW EXPLOIT TEST ===")
	fmt.Println("os.Chdir(projectRoot(t))", projectRoot(t), os.Chdir(projectRoot(t)))

	// Use pre-compiled WASM (compiled with gasUnderflow function)
	WASM_PATH := projectRoot(t) + "/modules/wasm/e2e/tmp/main.wasm"
	WASM_TEST_CODE, err := os.ReadFile(WASM_PATH)
	if err != nil {
		t.Fatalf("Failed to read WASM file: %v (run tinygo build first)", err)
	}

	contractId := "vsc-exploit-test"
	txSelf := stateEngine.TxSelf{
		TxId:                 "exploit-tx-001",
		BlockId:              "abcdef",
		Index:                0,
		OpIndex:              0,
		Timestamp:            "2026-03-28T00:00:00",
		RequiredAuths:        []string{"hive:attacker"},
		RequiredPostingAuths: []string{},
	}

	ct := test_utils.NewContractTest()
	ct.RegisterContract(contractId, "hive:attacker", WASM_TEST_CODE[:])

	// === TEST 1: Call gasUnderflow with rc_limit=1000 ===
	// Gas budget = 1000 * 100,000 = 100,000,000
	// The exploit writes 200 bytes costing 380,000,000 gas — should exceed budget
	fmt.Println("\n--- Calling gasUnderflow with rc_limit=1000 ---")
	fmt.Println("Gas budget: 100,000,000")
	fmt.Println("Expected cost of 200-byte write: ~380,000,000 (3.8x budget)")

	result := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "gasUnderflow",
		Payload:    json.RawMessage([]byte("")),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})

	fmt.Println("\n--- RESULTS ---")
	fmt.Printf("Success:  %v\n", result.Success)
	fmt.Printf("Return:   %s\n", result.Ret)
	fmt.Printf("RcUsed:   %d\n", result.RcUsed)
	fmt.Printf("GasUsed:  %d\n", result.GasUsed)
	fmt.Printf("Error:    %s\n", result.Err)
	fmt.Printf("ErrMsg:   %s\n", result.ErrMsg)

	if result.Success {
		fmt.Println("\n!!! EXPLOIT CONFIRMED !!!")
		fmt.Println("Contract executed BEYOND gas budget and succeeded.")
		fmt.Printf("Gas budget was 100,000,000 but contract used %d gas\n", result.GasUsed)
		fmt.Printf("That is %.1fx the allowed budget\n", float64(result.GasUsed)/100_000_000.0)

		// Check if post-underflow state writes persisted
		fmt.Println("\n--- Checking state writes ---")
		proof1 := ct.StateGet(contractId, "poc-proof1")
		proof2 := ct.StateGet(contractId, "poc-proof2")
		proof3 := ct.StateGet(contractId, "poc-proof3")
		spam0 := ct.StateGet(contractId, "poc-spam-0")
		spam19 := ct.StateGet(contractId, "poc-spam-19")

		fmt.Printf("poc-proof1: %q\n", proof1)
		fmt.Printf("poc-proof2: %q\n", proof2)
		fmt.Printf("poc-proof3: %q\n", proof3)
		fmt.Printf("poc-spam-0: %q\n", spam0)
		fmt.Printf("poc-spam-19: %q\n", spam19)

		if proof1 == "still-running" {
			fmt.Println("\n!!! STATE WRITES PERSISTED AFTER GAS EXHAUSTION !!!")
			fmt.Println("This proves the gas underflow allows unlimited state writes.")
		}

		// Count total state mutations in the diff
		if diff, ok := result.StateDiff[contractId]; ok {
			fmt.Printf("\nTotal state keys written: %d\n", len(diff.KeyDiff))
		}

		// Print logs
		if logs, ok := result.Logs[contractId]; ok {
			fmt.Println("\n--- Contract logs ---")
			for _, log := range logs.Logs {
				fmt.Printf("  LOG: %s\n", log)
			}
		}
	} else {
		fmt.Println("\nExploit BLOCKED — contract was stopped before completing.")
		fmt.Println("Gas enforcement is working correctly.")
	}

	// === VERDICT ===
	fmt.Println("\n=== VERDICT ===")
	if result.Success && result.Ret == "EXPLOIT_SUCCESS" {
		t.Log("CRITICAL: Gas underflow exploit is CONFIRMED. Contract ran with unlimited gas.")
		// We intentionally don't fail the test — we want to see the output
		assert.True(t, result.GasUsed > 100_000_000, "Gas used should exceed budget")
	} else {
		t.Log("Gas enforcement appears to be working. Exploit was blocked.")
	}
}
