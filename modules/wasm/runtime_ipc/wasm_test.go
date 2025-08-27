package wasm_runtime_test

import (
	_ "embed" // Important: import the embed package
	"fmt"

	"context"
	"encoding/hex"
	"math"
	"testing"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	"vsc-node/modules/db/vsc/contracts"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime "vsc-node/modules/wasm/runtime"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"

	"github.com/stretchr/testify/assert"
)

//NOTE: AssemblyScript will be furloughed until further notice
/*
import { db } from "@vsc.eco/sdk/assembly";

	export function main(args: string): string {
	  return db.getObject(args);
	}
*/
//go:embed artifacts/ASSEMBLYSCRIPT.wasm
var ASSEMBLY_SCRIPT_TEST_CODE []byte

/*
package main

import "contract-template/sdk"

//go:wasmexport entrypoint
func Entrypoint(a *string) *string {
	return sdk.GetObject(a)
}
*/
//go:embed artifacts/GO_TEST.wasm
var GO_TEST_CODE []byte

//NOTE: AssemblyScript will be furloughed until further notice
/*
import { db, console, Crypto, SystemAPI, getEnv } from "@vsc.eco/sdk/assembly";

	export function main(arg: string): string {
	  console.log("strlog");
	  const arr = new Uint8Array(3);
	  arr[0] = 27;
	  arr[1] = 69;
	  arr[2] = 66;
	  db.setObject("dbKey", "dbValue");
	  assert(db.getObject("dbKey") === "dbValue");
	  Crypto.sha256(arr); // TODO assert
	  Crypto.ripemd160(arr); // TODO assert
	  getEnv();
	  SystemAPI.call("syscallFuncName", '{"arg0":"argValue"}');
	  return "result value";
	}
*/

//go:embed artifacts/SDK_TEST.wasm
var SDK_TEST_CODE []byte

//NOTE: AssemblyScript will be furloughed until further notice. We will NOT be supporting AssemblyScript for the
/*
import { db, console, Crypto, SystemAPI, getEnv } from "@vsc.eco/sdk/assembly";

	export function main(arg: string): string {
	  console.log("strlog");
	  const arr = new Uint8Array(3);
	  arr[0] = 27;
	  arr[1] = 69;
	  arr[2] = 66;
	  db.setObject("dbKey", "dbValue");
	  Crypto.sha256(arr); // TODO assert
	  Crypto.ripemd160(arr); // TODO assert
	  getEnv();
	  SystemAPI.call("syscallFuncName", '{"arg0":"argValue"}');
	  return "result value";
	}
*/
//go:embed artifacts/SDK_TEST2.wasm
var SDK_TEST_CODE_2 []byte

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

const asGas = 14845

// const asGas = 1465500000000

func TestGoCompat(t *testing.T) {
	w := wasm_runtime_ipc.New()

	test_utils.RunPlugin(t, w)

	stateStore := test_utils.NewInMemoryStateStore()

	key := "_📝_testing_すし_"
	value := key
	stateStore.Set(key, []byte(value))

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
		int64(math.Ceil(float64(goGas+ioGas)/common.CYCLE_GAS_PER_RC)),
		nil,
		stateStore,
		contracts.ContractMetadata{},
	)
	ctx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(GO_TEST_CODE))

	res := w.Execute(ctx, goGas+uint(ioGas), "entrypoint", key, wasm_runtime.Go)

	fmt.Println("res:", *res.Error, res.Result)
	// in := NewBuffer()
	// out := NewBuffer()

	// res, err := promise.All(ctx,
	// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
	// 		host := stdio_ipc.NewJsonConnection(in, out, messages.MessageTypes[wasm_runtime_ipc.WasmResultStruct]()).Unwrap()
	// 		resolve(ipc_host.ExecuteCommand(ctx, host).MapErr(func(err error) error {
	// 			reject(err)
	// 			return nil
	// 		}).Unwrap())
	// 	}),
	// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
	// 		client := ipc_client.RunWithStdio[wasm_runtime_ipc.WasmResultStruct](in, out)
	// 		resolve(w.ExecuteWithClient(goGas+uint(ioGas), "entrypoint", key, wasm_runtime.Go, client).MapErr(func(err error) error {
	// 			reject(err)
	// 			return nil
	// 		}).Unwrap())
	// 	}),
	// ).Await(ctx)

	assert.Equal(t, ioGas, ctxValue.IOGas())

	// assert.NoError(t, err)
	assert.NotNil(t, res)
	// assert.Equal(t, value, (*res)[1].Result)
}

func TestAssemblyScriptCompat(t *testing.T) {
	w := wasm_runtime_ipc.New()

	test_utils.RunPlugin(t, w)

	stateStore := test_utils.NewInMemoryStateStore()

	key := "_📝_testing_すし_"
	value := key
	stateStore.Set(key, []byte(value))

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
		int64(math.Ceil(float64(asGas+ioGas)/common.CYCLE_GAS_PER_RC)),
		nil,
		stateStore,
		contracts.ContractMetadata{},
	)
	ctx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(ASSEMBLY_SCRIPT_TEST_CODE))

	res := w.Execute(ctx, asGas+uint(ioGas), "main", key, wasm_runtime.AssemblyScript)

	fmt.Println("res:", res)
	// in := NewBuffer()
	// out := NewBuffer()

	// res, err := promise.All(ctx,
	// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
	// 		host := stdio_ipc.NewJsonConnection(in, out, messages.MessageTypes[wasm_runtime_ipc.WasmResultStruct]()).Unwrap()
	// 		resolve(ipc_host.ExecuteCommand(ctx, host).MapErr(func(err error) error {
	// 			reject(err)
	// 			return nil
	// 		}).Unwrap())
	// 	}),
	// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
	// 		client := ipc_client.RunWithStdio[wasm_runtime_ipc.WasmResultStruct](in, out)
	// 		resolve(w.ExecuteWithClient(asGas+uint(ioGas), "main", key, wasm_runtime.AssemblyScript, client).MapErr(func(err error) error {
	// 			reject(err)
	// 			return nil
	// 		}).Unwrap())
	// 	}),
	// ).Await(ctx)

	assert.Equal(t, ioGas, ctxValue.IOGas())

	// assert.NoError(t, err)
	assert.NotNil(t, res)
	// assert.Equal(t, value, (*res)[1].Result)
}

// B/op / ns/op * 1_000_000_000ns/sec
// i5 8250U => 1.46 billion gas per sec
// i5 12400 => 6.03 billion gas per sec
func BenchmarkInstructionsPerSecond(b *testing.B) {
	tests := []struct {
		code       []byte
		runtime    wasm_runtime.Runtime
		maxGas     uint
		entrypoint string
	}{
		{
			code:       ASSEMBLY_SCRIPT_TEST_CODE,
			runtime:    wasm_runtime.AssemblyScript,
			maxGas:     asGas,
			entrypoint: "main",
		},
		{
			code:       GO_TEST_CODE,
			runtime:    wasm_runtime.Go,
			maxGas:     goGas,
			entrypoint: "entrypoint",
		},
	}
	for _, test := range tests {
		for i := 0; i < b.N; i++ {

			w := wasm_runtime_ipc.New()

			test_utils.RunPlugin(b, w)

			stateStore := test_utils.NewInMemoryStateStore()

			key := "_📝_testing_すし_"
			value := key
			stateStore.Set(key, []byte(value))

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
				int64(math.Ceil(float64(goGas+ioGas)/common.CYCLE_GAS_PER_RC)),
				nil,
				stateStore,
				contracts.ContractMetadata{},
			)
			ctx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(test.code))

			res := w.Execute(ctx, test.maxGas+uint(ioGas), test.entrypoint, key, test.runtime)

			fmt.Println("res:", *res.Error, res.Result)

			// in := NewBuffer()
			// out := NewBuffer()

			// res, err := promise.All(ctx,
			// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
			// 		host := stdio_ipc.NewJsonConnection(in, out, messages.MessageTypes[wasm_runtime_ipc.WasmResultStruct]()).Unwrap()
			// 		resolve(ipc_host.ExecuteCommand(ctx, host).MapErr(func(err error) error {
			// 			reject(err)
			// 			return nil
			// 		}).Unwrap())
			// 	}),
			// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
			// 		client := ipc_client.RunWithStdio[wasm_runtime_ipc.WasmResultStruct](in, out)
			// 		resolve(w.ExecuteWithClient(test.maxGas+uint(ioGas), test.entrypoint, key, test.runtime, client).MapErr(func(err error) error {
			// 			reject(err)
			// 			return nil
			// 		}).Unwrap())
			// 	}),
			// ).Await(ctx)

			// assert.NoError(b, err)
			assert.NotNil(b, res)

			// b.SetBytes(int64((*res)[1].Gas))
		}
	}
}

const sdkGas = 1_000_000

func TestSdkCompat(t *testing.T) {
	ioGas := 23900000000
	w := wasm_runtime_ipc.New()

	test_utils.RunPlugin(t, w)

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
		int64(math.Ceil(float64(sdkGas+ioGas)/common.CYCLE_GAS_PER_RC)),
		nil,
		stateStore,
		contracts.ContractMetadata{},
	)
	ctx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(SDK_TEST_CODE_2))

	// in := NewBuffer()
	// out := NewBuffer()

	// var finished func(wasm_types.WasmResultStruct)

	res := w.Execute(ctx, sdkGas+uint(ioGas), "main", "testing-123", wasm_runtime.Go)

	fmt.Println("res:", *res.Error, res.Result)

	// res, err := promise.All(ctx,
	// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
	// 		host := stdio_ipc.NewJsonConnection(in, out, messages.MessageTypes[wasm_runtime_ipc.WasmResultStruct]()).Unwrap()
	// 		res := ipc_host.ExecuteCommand(ctx, host).MapErr(func(err error) error {
	// 			reject(err)
	// 			return nil
	// 		}).Unwrap()
	// 		resolve(res)
	// 		finished(res)
	// 	}),
	// 	promise.New(func(resolve func(wasm_runtime_ipc.WasmResultStruct), reject func(error)) {
	// 		client := ipc_client.RunWithStdio[wasm_runtime_ipc.WasmResultStruct](in, out)
	// 		finished = func(wrs wasm_types.WasmResultStruct) {
	// 			resolve(wrs)
	// 		}
	// 		resolve(w.ExecuteWithClient(sdkGas+uint(ioGas), "main", "testing-123", wasm_runtime.AssemblyScript, client).MapErr(func(err error) error {
	// 			reject(err)
	// 			return nil
	// 		}).Unwrap())
	// 	}),
	// ).Await(ctx)

	assert.Equal(t, "dbValue", string(stateStore.Get("dbKey")))

	// assert.NoError(t, err)
	assert.NotNil(t, res)
	// assert.True(t, (*res)[1].Error)
	// assert.Equal(t, "INVALID_CALL", (*res)[1].Result)
	assert.Equal(t, ioGas, ctxValue.IOGas())

}
