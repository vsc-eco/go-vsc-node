package wasm_e2e

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	"vsc-node/modules/db/vsc/contracts"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime "vsc-node/modules/wasm/runtime"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"
)

type Buffer struct {
	c chan byte
	// *io.PipeReader
	// *io.PipeWriter
}

func NewBuffer() *Buffer {
	return &Buffer{c: make(chan byte, 8*1024*1024)} // TODO by making the buffer size very large, the test will no longer be flaky. Does this indicate a likely problem in production?
	// r, w := io.Pipe()
	// return &Buffer{r, w}
}

func (b *Buffer) Read(p []byte) (n int, err error) {
	for i := 0; i < len(p); i++ {
		if len(b.c) == 0 {
			return i, nil
		}
		p[i] = <-b.c
	}
	return len(p), nil
}

func (b *Buffer) Write(p []byte) (n int, err error) {
	for i, v := range p {
		if len(b.c) == cap(b.c) {
			return i, nil
		}
		b.c <- v
	}
	return len(p), nil
}

const ioGas = 4200000000

const goGas = 704

func TestCompileAndExecute(t *testing.T) {
	w := wasm_runtime_ipc.New()
	w.Init()

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

	entrypoint := "callfunc"

	stateStore := test_utils.NewInMemoryStateStore()

	key := "_📝_testing_すし_"
	value := key
	stateStore.Set(key, []byte(value))

	radicalStructure := map[string]any{
		"key":   key,
		"value": value,
	}
	// meh, _ := common.EncodeDagCbor(radicalStructure)
	meh, _ := json.Marshal(radicalStructure)

	var ctxValue wasm_context.ExecContextValue = contract_execution_context.New(
		contract_execution_context.Environment{
			ContractId:  "vsc1Bak1RGMgUxvLriSCoLJWZ5Ghp26ci2CiSk",
			BlockHeight: 97939313,
			TxId:        "1b041f35ae729198375551b77d9ec20d00b3bc9b",
			BlockId:     "05d66f716d4573aeb74d8b6533d3c804a5e98cae",
			Index:       23,
			OpIndex:     2,
			Timestamp:   "2025-07-25T00:44:24",
			RequiredAuths: []string{
				"hive:vaultec.test",
			},
			RequiredPostingAuths: []string{},
			Caller:               "hive:vaultec.test",
			Intents: []contracts.Intent{{
				Type: "transfer.allow",
				Args: map[string]string{
					"limit": "1.000",
					"token": "hive",
				},
			}},
		},
		int64(math.Ceil(float64(goGas+ioGas)/common.CYCLE_GAS_PER_RC)),
		nil,
		stateStore,
		contracts.ContractMetadata{},
	)
	ctx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(WASM_TEST_CODE))

	w.Execute(ctx, goGas+uint(ioGas), entrypoint, string(meh), wasm_runtime.Go)
	basicErrorResult := w.Execute(ctx, 5000+uint(ioGas), entrypoint, "testing-123", wasm_runtime.Go)

	if basicErrorResult.Error != nil {
		fmt.Println("Error executing WASM:", *basicErrorResult.Error)
	} else {
		fmt.Println("basicErrorResult:", basicErrorResult.Result, "gas="+strconv.Itoa(int(basicErrorResult.Result.Gas)))
	}

	fmt.Println("hello world!")

}
