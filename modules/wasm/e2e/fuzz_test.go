package wasm_e2e

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	stateEngine "vsc-node/modules/state-processing"
)

// callWithRecover calls a contract action with the given payload, recovering from panics.
// Returns (result, panicValue). If panicValue != nil, a panic occurred.
func callWithRecover(ct test_utils.ContractTest, contractId string, action string, payload string, txSelf stateEngine.TxSelf) (result test_utils.ContractTestCallResult, panicVal interface{}) {
	defer func() {
		if r := recover(); r != nil {
			panicVal = r
		}
	}()
	result = ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     action,
		Payload:    json.RawMessage([]byte(payload)),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	return result, nil
}

func callWithRecoverRc(ct test_utils.ContractTest, contractId string, action string, payload string, txSelf stateEngine.TxSelf, rcLimit uint) (result test_utils.ContractTestCallResult, panicVal interface{}) {
	defer func() {
		if r := recover(); r != nil {
			panicVal = r
		}
	}()
	result = ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     action,
		Payload:    json.RawMessage([]byte(payload)),
		RcLimit:    rcLimit,
		Intents:    []contracts.Intent{},
	})
	return result, nil
}

// knownActions are the exported functions in the test WASM contract.
var knownActions = []string{
	"setString",
	"getString",
	"clearString",
	"setEphemStr",
	"drawHive",
	"dumpEnv",
	"dumpEnvKey",
	"abortMe",
	"revertMe",
	"contractGetString",
	"contractCall",
	"infiniteRecursion",
}

func makeAuthList(n int) []string {
	auths := make([]string, n)
	for i := 0; i < n; i++ {
		auths[i] = fmt.Sprintf("hive:user%d", i)
	}
	return auths
}

// TestFuzzAll runs all fuzz test categories using a single ContractTest instance
// to avoid badger directory lock contention across test functions.
func TestFuzzAll(t *testing.T) {
	fmt.Println("=== VSC WASM SDK FUZZ TESTS ===")
	fmt.Println("os.Chdir(projectRoot(t))", projectRoot(t), os.Chdir(projectRoot(t)))

	WASM_PATH := projectRoot(t) + "/modules/wasm/e2e/tmp/main.wasm"
	WASM_TEST_CODE, err := os.ReadFile(WASM_PATH)
	if err != nil {
		t.Fatalf("Failed to read WASM file: %v", err)
	}

	contractId := "vsc-fuzz-target"
	txSelf := stateEngine.TxSelf{
		TxId:                 "fuzz-tx-001",
		BlockId:              "abcdef",
		Index:                0,
		OpIndex:              0,
		Timestamp:            "2026-03-28T00:00:00",
		RequiredAuths:        []string{"hive:fuzzer"},
		RequiredPostingAuths: []string{},
	}

	ct := test_utils.NewContractTest()
	ct.RegisterContract(contractId, "hive:fuzzer", WASM_TEST_CODE[:])

	totalPanics := []string{}

	// --- 1: Empty/nil payloads ---
	t.Run("EmptyNilPayloads", func(t *testing.T) {
		payloads := []struct {
			name string
			data string
		}{
			{"empty_string", ""},
			{"null_literal", "null"},
			{"empty_object", "{}"},
			{"empty_array", "[]"},
			{"bare_quotes", `""`},
		}

		for _, action := range knownActions {
			for _, p := range payloads {
				label := fmt.Sprintf("%s/%s", action, p.name)
				t.Run(label, func(t *testing.T) {
					result, panicVal := callWithRecover(ct, contractId, action, p.data, txSelf)
					if panicVal != nil {
						msg := fmt.Sprintf("PANIC in %s with payload %q: %v", action, p.data, panicVal)
						totalPanics = append(totalPanics, msg)
						t.Errorf("%s", msg)
					} else {
						fmt.Printf("  %-25s payload=%-12s success=%v err=%s\n", action, p.name, result.Success, result.Err)
					}
				})
			}
		}
	})

	// --- 2: Huge payloads ---
	t.Run("HugePayloads", func(t *testing.T) {
		hugeStr := strings.Repeat("A", 100*1024)
		hugePayload := fmt.Sprintf(`"%s"`, hugeStr)

		for _, action := range knownActions {
			label := fmt.Sprintf("%s/100KB", action)
			t.Run(label, func(t *testing.T) {
				result, panicVal := callWithRecover(ct, contractId, action, hugePayload, txSelf)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC in %s with 100KB payload: %v", action, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
				} else {
					fmt.Printf("  %-25s payload=100KB  success=%v err=%s\n", action, result.Success, result.Err)
				}
			})
		}
	})

	// --- 3: Invalid JSON ---
	t.Run("InvalidJSON", func(t *testing.T) {
		invalidPayloads := []struct {
			name string
			data string
		}{
			{"unclosed_brace", "{invalid"},
			{"quad_braces", "{{{{"},
			{"trailing_comma", `{"a":1,}`},
			{"single_quotes", "{'key': 'val'}"},
			{"raw_text", "not json at all"},
			{"unquoted_key", "{key: value}"},
			{"truncated_string", `"unterminated`},
			{"backslash_hell", `"\\\\\\\"`},
			{"unicode_garbage", "\xef\xbf\xbd\xc0\xaf\xe0\x80\x80"},
			{"null_bytes_json", "{\"k\": \"v\x00\x00\x00\"}"},
		}

		for _, action := range knownActions {
			for _, p := range invalidPayloads {
				label := fmt.Sprintf("%s/%s", action, p.name)
				t.Run(label, func(t *testing.T) {
					result, panicVal := callWithRecover(ct, contractId, action, p.data, txSelf)
					if panicVal != nil {
						msg := fmt.Sprintf("PANIC in %s with invalid JSON %q: %v", action, p.name, panicVal)
						totalPanics = append(totalPanics, msg)
						t.Errorf("%s", msg)
					} else {
						fmt.Printf("  %-25s payload=%-20s success=%v err=%s\n", action, p.name, result.Success, result.Err)
					}
				})
			}
		}
	})

	// --- 4: Integer boundary values ---
	t.Run("IntegerBoundaries", func(t *testing.T) {
		intPayloads := []struct {
			name string
			data string
		}{
			{"zero", "0"},
			{"negative_one", "-1"},
			{"huge_int", "99999999999999999999999999"},
			{"nan", `"NaN"`},
			{"infinity", `"Infinity"`},
			{"neg_infinity", `"-Infinity"`},
			{"max_int64", "9223372036854775807"},
			{"min_int64", "-9223372036854775808"},
			{"overflow_int64", "9223372036854775808"},
			{"float", "1.5"},
			{"neg_float", "-0.001"},
			{"scientific", "1e308"},
			{"neg_scientific", "-1e308"},
		}

		numericActions := []string{"drawHive", "setString", "dumpEnvKey", "abortMe"}

		for _, action := range numericActions {
			for _, p := range intPayloads {
				label := fmt.Sprintf("%s/%s", action, p.name)
				t.Run(label, func(t *testing.T) {
					result, panicVal := callWithRecover(ct, contractId, action, p.data, txSelf)
					if panicVal != nil {
						msg := fmt.Sprintf("PANIC in %s with payload %q (%s): %v", action, p.data, p.name, panicVal)
						totalPanics = append(totalPanics, msg)
						t.Errorf("%s", msg)
					} else {
						fmt.Printf("  %-25s payload=%-30s success=%v err=%s\n", action, p.name, result.Success, result.Err)
					}
				})
			}
		}
	})

	// --- 5: Special characters ---
	t.Run("SpecialCharacters", func(t *testing.T) {
		specialPayloads := []struct {
			name string
			data string
		}{
			{"null_bytes", "key\x00value"},
			{"newlines", "key\n\r\nvalue"},
			{"tabs", "key\t\tvalue"},
			{"unicode_emoji", `"` + "\U0001F4A9\U0001F525\U0001F680" + `"`},
			{"unicode_rtl", `"` + "\u200F\u200E\u202A\u202B" + `"`},
			{"unicode_zalgo", `"Z̴̡̢̛̰̗̮̣̦̜̖̪̲̙͔̫̩̜̙̪̪̫̰̤̺̥̠̣̼̫̪͕̦̪̫̭̗̤̙̰̬̰̣̩̖̫̥̤̣̮̞̮̦̫̘̠̈̇̂̐̊̿̓̿̓́̽́̋̋̿̋̅̀̈́̂̓̋̇̈́̽̿́̄̂̂̋̓̎̋̈́̽̌̑̓̀̒̍̐̊͊̂̏̈́̕̕͜͠͝͠ͅa̵l̷g̵o̶"`},
			{"control_chars", "\x01\x02\x03\x04\x05\x06\x07\x08"},
			{"backspace_del", "\x08\x7f"},
			{"form_feed", "\x0c"},
			{"long_unicode", strings.Repeat("\U0001F600", 1000)},
		}

		for _, action := range knownActions {
			for _, p := range specialPayloads {
				label := fmt.Sprintf("%s/%s", action, p.name)
				t.Run(label, func(t *testing.T) {
					result, panicVal := callWithRecover(ct, contractId, action, p.data, txSelf)
					if panicVal != nil {
						msg := fmt.Sprintf("PANIC in %s with special chars %q: %v", action, p.name, panicVal)
						totalPanics = append(totalPanics, msg)
						t.Errorf("%s", msg)
					} else {
						fmt.Printf("  %-25s payload=%-20s success=%v err=%s\n", action, p.name, result.Success, result.Err)
					}
				})
			}
		}
	})

	// --- 6: Non-existent functions ---
	t.Run("NonExistentFunctions", func(t *testing.T) {
		fakeActions := []string{
			"",
			"a",
			strings.Repeat("x", 10000),
			"__proto__",
			"constructor",
			"toString",
			"valueOf",
			"hasOwnProperty",
			"../../../etc/passwd",
			"system.call",
			"hive.transfer",
			"db.set",
			"db.get",
			"setString\x00extradata",
			"\n\r\t",
			"SELECT * FROM users",
			"<script>alert(1)</script>",
		}

		for _, action := range fakeActions {
			displayName := action
			if len(displayName) > 40 {
				displayName = displayName[:40] + "..."
			}
			label := fmt.Sprintf("action=%q", displayName)
			t.Run(label, func(t *testing.T) {
				result, panicVal := callWithRecover(ct, contractId, action, `"test"`, txSelf)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC calling non-existent action %q: %v", displayName, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
				} else {
					fmt.Printf("  action=%-45q success=%v err=%s\n", displayName, result.Success, result.Err)
				}
			})
		}
	})

	// --- 7: Rapid sequential calls ---
	t.Run("RapidSequentialCalls", func(t *testing.T) {
		const iterations = 100

		t.Run("RapidSetString", func(t *testing.T) {
			for i := 0; i < iterations; i++ {
				payload := fmt.Sprintf("key%d,value%d", i, i)
				result, panicVal := callWithRecover(ct, contractId, "setString", payload, txSelf)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC at iteration %d (setString payload=%q): %v", i, payload, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
					return
				}
				if !result.Success {
					t.Logf("setString failed at iteration %d: err=%s", i, result.Err)
				}
			}
			// Verify state integrity
			for _, checkIdx := range []int{0, 49, 99} {
				key := fmt.Sprintf("key%d", checkIdx)
				expected := fmt.Sprintf("value%d", checkIdx)
				got := ct.StateGet(contractId, key)
				if got != expected {
					t.Errorf("State corruption: key=%s expected=%q got=%q", key, expected, got)
				}
			}
		})

		t.Run("RapidDumpEnv", func(t *testing.T) {
			for i := 0; i < iterations; i++ {
				_, panicVal := callWithRecover(ct, contractId, "dumpEnv", "", txSelf)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC at iteration %d (dumpEnv): %v", i, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
					return
				}
			}
		})

		t.Run("RapidNonExistent", func(t *testing.T) {
			for i := 0; i < iterations; i++ {
				action := fmt.Sprintf("fake_%d", i)
				_, panicVal := callWithRecover(ct, contractId, action, `"x"`, txSelf)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC at iteration %d (action=%s): %v", i, action, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
					return
				}
			}
		})

		t.Run("RapidAbort", func(t *testing.T) {
			for i := 0; i < iterations; i++ {
				_, panicVal := callWithRecover(ct, contractId, "abortMe", fmt.Sprintf("abort-%d", i), txSelf)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC at iteration %d (abortMe): %v", i, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
					return
				}
			}
		})
	})

	// --- 8: Adversarial TxSelf fields ---
	t.Run("AdversarialTxSelf", func(t *testing.T) {
		adversarialTxSelves := []struct {
			name   string
			txSelf stateEngine.TxSelf
		}{
			{
				"empty_required_auths",
				stateEngine.TxSelf{
					TxId: "fuzz-tx", BlockId: "abcdef", Timestamp: "2026-03-28T00:00:00",
					RequiredAuths: []string{}, RequiredPostingAuths: []string{},
				},
			},
			{
				"nil_required_auths",
				stateEngine.TxSelf{
					TxId: "fuzz-tx", BlockId: "abcdef", Timestamp: "2026-03-28T00:00:00",
					RequiredAuths: nil, RequiredPostingAuths: nil,
				},
			},
			{
				"empty_string_auth",
				stateEngine.TxSelf{
					TxId: "fuzz-tx", BlockId: "abcdef", Timestamp: "2026-03-28T00:00:00",
					RequiredAuths: []string{""}, RequiredPostingAuths: []string{},
				},
			},
			{
				"empty_tx_fields",
				stateEngine.TxSelf{
					RequiredAuths: []string{"hive:fuzzer"}, RequiredPostingAuths: []string{},
				},
			},
			{
				"negative_index",
				stateEngine.TxSelf{
					TxId: "fuzz-tx", BlockId: "abcdef", Index: -1, OpIndex: -1,
					Timestamp: "2026-03-28T00:00:00",
					RequiredAuths: []string{"hive:fuzzer"}, RequiredPostingAuths: []string{},
				},
			},
			{
				"huge_index",
				stateEngine.TxSelf{
					TxId: "fuzz-tx", BlockId: "abcdef", Index: 999999999, OpIndex: 999999999,
					Timestamp: "2026-03-28T00:00:00",
					RequiredAuths: []string{"hive:fuzzer"}, RequiredPostingAuths: []string{},
				},
			},
			{
				"invalid_timestamp",
				stateEngine.TxSelf{
					TxId: "fuzz-tx", BlockId: "abcdef", Timestamp: "not-a-date",
					RequiredAuths: []string{"hive:fuzzer"}, RequiredPostingAuths: []string{},
				},
			},
			{
				"many_auths",
				stateEngine.TxSelf{
					TxId: "fuzz-tx", BlockId: "abcdef", Timestamp: "2026-03-28T00:00:00",
					RequiredAuths: makeAuthList(1000), RequiredPostingAuths: []string{},
				},
			},
		}

		for _, tc := range adversarialTxSelves {
			t.Run(tc.name, func(t *testing.T) {
				result, panicVal := callWithRecover(ct, contractId, "dumpEnv", "", tc.txSelf)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC with adversarial TxSelf %q: %v", tc.name, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
				} else {
					fmt.Printf("  %-25s success=%v err=%s\n", tc.name, result.Success, result.Err)
				}
			})
		}
	})

	// --- 9: RcLimit edge cases ---
	t.Run("RcLimitEdgeCases", func(t *testing.T) {
		rcLimits := []struct {
			name  string
			limit uint
		}{
			{"zero", 0},
			{"one", 1},
			{"max_uint32", 4294967295},
			{"max_uint", ^uint(0)},
		}

		for _, rc := range rcLimits {
			t.Run(rc.name, func(t *testing.T) {
				_, panicVal := callWithRecoverRc(ct, contractId, "dumpEnv", "", txSelf, rc.limit)
				if panicVal != nil {
					msg := fmt.Sprintf("PANIC with RcLimit=%v (%s): %v", rc.limit, rc.name, panicVal)
					totalPanics = append(totalPanics, msg)
					t.Errorf("%s", msg)
				} else {
					fmt.Printf("  RcLimit=%-25s no panic\n", rc.name)
				}
			})
		}
	})

	// --- Final summary ---
	fmt.Println("\n========================================")
	fmt.Println("=== FUZZ TEST SUMMARY ===")
	fmt.Println("========================================")
	if len(totalPanics) > 0 {
		fmt.Printf("PANICS FOUND: %d\n", len(totalPanics))
		for i, p := range totalPanics {
			fmt.Printf("  %d. %s\n", i+1, p)
		}
	} else {
		fmt.Println("RESULT: ZERO PANICS across all fuzz categories")
		fmt.Println("All adversarial inputs were handled gracefully (errors returned, no crashes)")
	}
	fmt.Println("========================================")
}
