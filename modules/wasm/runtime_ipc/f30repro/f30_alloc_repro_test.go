// Package f30repro hosts the end-to-end reproduction of pentest
// finding F30. It lives in its own directory (away from
// modules/e2e, which has unrelated build issues against the
// missing evm_mapping.wasm artifact) so the F30 reproduction
// can be exercised in isolation by `go test ./modules/wasm/runtime_ipc/f30repro/...`.
package f30repro

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	stateEngine "vsc-node/modules/state-processing"
)

// loadContractWasm finds and reads the existing tracked test
// contract at modules/e2e/artifacts/contract_test.wasm. We resolve
// it relative to this source file rather than embedding it so the
// 170 KB binary doesn't need to be duplicated into this package.
func loadContractWasm(t *testing.T) []byte {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller(0) failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate project root (go.mod)")
		}
		dir = parent
	}
	path := filepath.Join(dir, "modules", "e2e", "artifacts", "contract_test.wasm")
	bytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if len(bytes) == 0 {
		t.Fatalf("contract_test.wasm at %s is empty — reproduction needs the real TinyGo build", path)
	}
	return bytes
}

// End-to-end reproduction of pentest finding F30.
//
// Bug: Wasm.Execute (modules/wasm/runtime_ipc/wasm.go) forwards the
// user-supplied entrypoint (= TxVscCallContract.Action) directly to
// wasmedge.ExecuteRegistered("contract", entrypoint, ...) without
// any allowlist. TinyGo-compiled contracts on Magi expose runtime-
// internal symbols like `alloc`, which the dispatcher will then
// happily invoke on behalf of any anonymous account, burning gas
// against every deployed contract.
//
// This test uses the existing modules/e2e/artifacts/contract_test.wasm
// (a real TinyGo build that exports `alloc`, `_initialize`, and
// `main`) and drives it through the full ContractTest.Call →
// Wasm.Execute pipeline.
//
// Pre-fix: Action="alloc" goes through. wasmedge runs alloc;
// result.Success is true, gas is consumed.
// Post-fix: Wasm.Execute rejects the entrypoint before any wasmedge
// state is allocated. Success is false, Err=WASM_FUNC_NOT_FND,
// GasUsed=0.
func TestF30_AllocActionRejectedAtDispatch(t *testing.T) {
	contractWasm := loadContractWasm(t)

	const owner = "hive:f30owner"
	const contractId = "vscf30test"

	ct := test_utils.NewContractTest()
	ct.Deposit(owner, 100_000, ledgerDb.AssetHive)
	ct.Deposit(owner, 100_000, ledgerDb.AssetHbd)
	ct.RegisterContract(contractId, owner, contractWasm)

	txSelf := stateEngine.TxSelf{
		TxId:                 "f30-tx",
		BlockId:              "f30-block",
		Index:                0,
		OpIndex:              0,
		Timestamp:            "2026-05-07T00:00:00",
		RequiredAuths:        []string{owner},
		RequiredPostingAuths: []string{},
	}

	// Sanity: a missing export returns WASM_FUNC_NOT_FND with no
	// gas consumed. This is the shape the forbidden-entrypoint
	// guard must produce on "alloc" too.
	control := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "thisFunctionDoesNotExist",
		Payload:    json.RawMessage([]byte(`""`)),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})
	if control.Success {
		t.Fatalf("control sanity check failed: nonexistent action should not succeed")
	}
	if control.Err != contracts.WASM_FUNC_NOT_FND {
		t.Fatalf("control: expected WASM_FUNC_NOT_FND for missing export, got %q (%s)",
			control.Err, control.ErrMsg)
	}

	// The actual F30 reproduction.
	allocResult := ct.Call(stateEngine.TxVscCallContract{
		Self:       txSelf,
		ContractId: contractId,
		Action:     "alloc",
		Payload:    json.RawMessage([]byte(`""`)),
		RcLimit:    1000,
		Intents:    []contracts.Intent{},
	})

	if allocResult.Success {
		t.Errorf("F30 bug reachable: Action=\"alloc\" succeeded against a deployed contract. "+
			"GasUsed=%d, Ret=%q", allocResult.GasUsed, allocResult.Ret)
	}
	if allocResult.Err != contracts.WASM_FUNC_NOT_FND {
		t.Errorf("F30 fix not in effect: alloc should be rejected as WASM_FUNC_NOT_FND, "+
			"got Err=%q ErrMsg=%q", allocResult.Err, allocResult.ErrMsg)
	}
	if allocResult.GasUsed != 0 {
		t.Errorf("F30 fix should reject before wasmedge setup (GasUsed=0), got GasUsed=%d",
			allocResult.GasUsed)
	}
}
