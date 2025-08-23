package wasm_parent_ipc_test

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"runtime"
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

const PROJECT_ROOT_NAME = "go-vsc-node"

func hasGoMod(dir string) bool {
	info, err := os.Stat(dir)
	if err != nil || !info.IsDir() {
		return false
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.Name() == "go.mod" {
			return true
		}
	}
	return false
}

func projectRoot(t *testing.T) string {
	_, file, _, ok := runtime.Caller(0)
	wd := file
	if !ok {
		t.Fatal("could not find root caller source file")
	}
	for !hasGoMod(wd) {
		prevWd := wd
		wd = filepath.Dir(wd)
		if wd == prevWd {
			t.Fatal("could not find root of project in this test. This is a bug with the test.")
		}
	}
	return wd
}

func TestAssemblyScriptCompat(t *testing.T) {
	err := os.Chdir(projectRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	// pwd, _ := os.Getwd()
	// fmt.Fprintln(os.Stderr, "pwd:", pwd)
	w := wasm_parent_ipc.New("go", "run", MAIN_PATH)
	// TODO attempt at running child process with debugger, but does not work :/
	// w := wasm_parent_ipc.New("~/go/bin/dlv", "--headless=true", "debug", MAIN_PATH)
	err = w.Init()
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Start().Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Wasm Execution times
	//0.00050701 seconds
	//0.000224499 seconds
	//0.00037473899999999996
	//0.00021495799999999998 seconds
	//0.000234295 seconds
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
	res := w.Execute(ctxValue, ASSEMBLY_SCRIPT_TEST_CODE, 14845, "main", "my-args-testing-123", wasm_runtime.AssemblyScript)
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

func TestGoCompat(t *testing.T) {
	err := os.Chdir(projectRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	// pwd, _ := os.Getwd()
	// fmt.Fprintln(os.Stderr, "pwd:", pwd)
	w := wasm_parent_ipc.New("go", "run", MAIN_PATH)
	// TODO attempt at running child process with debugger, but does not work :/
	// w := wasm_parent_ipc.New("~/go/bin/dlv", "--headless=true", "debug", MAIN_PATH)
	err = w.Init()
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Start().Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Wasm Execution times
	//0.00050701 seconds
	//0.000224499 seconds
	//0.00037473899999999996
	//0.00021495799999999998 seconds
	//0.000234295 seconds

	stateStore := test_utils.NewInMemoryStateStore()

	var ctxValue wasm_context.ExecContextValue = contract_execution_context.New(
		contract_execution_context.Environment{
			ContractId:           "",
			BlockHeight:          0,
			TxId:                 "",
			BlockId:              "",
			Index:                0,
			OpIndex:              0,
			Timestamp:            "",
			RequiredAuths:        []string{},
			RequiredPostingAuths: []string{},
		},
		int64(math.Ceil(float64(4845)/common.CYCLE_GAS_PER_RC)),
		nil,
		nil,
		stateStore,
		map[string]interface{}{},
	)
	fmt.Println("exec init:", time.Now())
	res := w.Execute(ctxValue, GO_TEST_CODE, 4845, "entrypoint", "my-args-testing-123", wasm_runtime.Go)
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
