package wasm_e2e

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"
	wasm_runtime "vsc-node/modules/wasm/runtime"
)

const MAIN_PATH = "vsc-node/cmd/vm-runner"

func TestCompileAndExecute(t *testing.T) {

	fmt.Println("os.Chdir(projectRoot(t))", projectRoot(t), os.Chdir(projectRoot(t)))
	wkdir := projectRoot(t)
	WASM_PATH, err := Compile(wkdir)
	if err != nil {
		t.Fatalf("Failed to compile: %v", err)
	}
	fmt.Println("WASM file compiled successfully:", WASM_PATH)
	WASM_TEST_CODE, err := os.ReadFile(WASM_PATH) // This is just to ensure the file exists and can be read.
	if err != nil {
		fmt.Println("Failed to read it")
		t.Fatalf("Failed to read WASM file: %v", err)
	}

	fmt.Println("WASM_TEST_CODE:", len(WASM_TEST_CODE), WASM_TEST_CODE[:10], "...")

	w := wasm_parent_ipc.New("go", "run", MAIN_PATH)
	// TODO attempt at running child process with debugger, but does not work :/
	// w := wasm_parent_ipc.New("~/go/bin/dlv", "--headless=true", "debug", MAIN_PATH)
	err = w.Init()
	if err != nil {
		fmt.Println("err:", err)
		t.Fatal(err)
	}
	_, err = w.Start().Await(context.Background())
	if err != nil {
		fmt.Println("err:", err)
		t.Fatal(err)
	}

	stateStore := test_utils.NewInMemoryStateStore()
	stateStore.Set("my-args-testing-123", []byte("my-testing-value"))
	var ctxValue wasm_context.ExecContextValue = contract_execution_context.New(
		contract_execution_context.Environment{},
		(14845)/common.CYCLE_GAS_PER_RC,
		nil,
		nil,
		stateStore,
		map[string]interface{}{})
	fmt.Println("exec init:", time.Now())
	res := w.Execute(ctxValue, WASM_TEST_CODE, 14845, "callfunc", "my-args-testing-123", wasm_runtime.Go)
	if res.IsOk() {
		t.Log("res:", res.Unwrap())
	} else {
		t.Fatal(res.UnwrapErr())
	}
	err = w.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
