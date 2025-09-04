package wasm_runtime

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"reflect"
	"unicode/utf16"
	ipc_client "vsc-node/lib/stdio-ipc/client"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"
	wasm_context "vsc-node/modules/wasm/context"
	"vsc-node/modules/wasm/ipc_requests"
	"vsc-node/modules/wasm/ipc_requests/execute"
	wasm_runtime "vsc-node/modules/wasm/runtime"
	"vsc-node/modules/wasm/sdk"
	sdkTypes "vsc-node/modules/wasm/sdk/types"
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
	"github.com/second-state/WasmEdge-go/wasmedge"
	// bindgen "github.com/second-state/wasmedge-bindgen/host/go"
)

type Wasm struct {
}

var _ a.Plugin = &Wasm{}

type WasmResultStruct = wasm_types.WasmResultStruct

type WasmResult = wasm_types.WasmResult

func New() *Wasm {
	return &Wasm{}
}

func (w *Wasm) Init() error {
	err := setup()
	if err != nil {
		return err
	}
	wasmedge.SetLogOff()
	return nil
}

func (w *Wasm) Start() *promise.Promise[any] {
	return utils.PromiseResolve[any](nil)
}

func (w *Wasm) Stop() error {
	return nil
}

func modCleanup(mod *wasmedge.Module) {
	for _, f := range mod.ListFunction() {
		mod.FindFunction(f).Release()
	}
	for _, g := range mod.ListGlobal() {
		mod.FindGlobal(g).Release()
	}
	mod.Release()
}

func resultToWasmEdgeResult(runtime wasm_runtime.Runtime, vm *wasmedge.VM, memory *wasmedge.Memory, res result.Result[string]) ([]interface{}, wasmedge.Result) {
	// res.InspectErr(func(err error) {
	// 	fmt.Println("err:", err)
	// })
	return result.MapOrElse(
			result.AndThen(
				res,
				func(t string) result.Result[int32] {
					return allocString(runtime, vm, memory, t)
				}),
			func(err error) []interface{} {
				errStr := err.Error()
				return []any{wasmedge.NewExternRef(&errStr)}
			},
			func(t int32) []interface{} {
				return []any{t}
			},
		),
		result.MapOr(
			res,
			wasmedge.Result_Fail,
			func(string) wasmedge.Result {
				return wasmedge.Result_Success
			},
		)
}

const ASSEMBLY_SCRIPT_STRING_ID = uint32(2)

func assemblyScriptAllocString(vm *wasmedge.VM, memory *wasmedge.Memory, t string) result.Result[int32] {
	// mod := vm.GetRegisteredModule("contract")
	return result.AndThen(
		resultWrap(encodeUtf16(t, binary.LittleEndian)),
		func(b []byte) result.Result[int32] {
			return result.AndThen(
				resultWrap(vm.ExecuteRegistered("contract", "__new", int32(2*len(t)), ASSEMBLY_SCRIPT_STRING_ID)),
				func(res []any) result.Result[int32] {
					if len(res) != 1 {
						return result.Err[int32](fmt.Errorf("invalid result when allocating memory for string"))
					}
					ptr, ok := res[0].(int32)
					if !ok {
						return result.Err[int32](fmt.Errorf("invalid result type when allocating memory for string"))
					}
					if memory == nil {
						mod := vm.GetRegisteredModule("env")
						memory = mod.FindMemory("memory")
					}
					return result.Map(
						resultWrap(memory.GetData(uint(ptr), uint(len(b)))),
						func(data []byte) int32 {
							copy(data, b)
							return ptr
						},
					)
				},
			)
		},
	)
}

// assumes value at address `ptr` is a valid AssemblyScript string
// meaning the header value `rtId` is 2
// https://www.assemblyscript.org/runtime.html#header-layout
// https://www.assemblyscript.org/runtime.html#class-layout
func assemblyScriptReadString(memory *wasmedge.Memory, ptr int32) result.Result[string] {
	return result.AndThen(
		result.AndThen(
			resultWrap(memory.GetData(uint(ptr)-4, 4)),
			func(sizeBytes []byte) result.Result[[]byte] {
				// FIXME assuming little endian for now
				size := binary.LittleEndian.Uint32(sizeBytes)
				return resultWrap(memory.GetData(uint(ptr), uint(size)))
			},
		),
		func(b []byte) result.Result[string] {
			// ble, _ := decodeUtf16(b, binary.LittleEndian)
			// fmt.Println("AssemblyScript Go:", b, string(b), ble)
			return result.Map(
				resultWrap(decodeUtf16(b, binary.LittleEndian)),
				func(s string) string {
					size := len(b) / 2
					return s[:size]
				},
			)
		},
	)
}

type dataType uint32

const (
	objectDataType dataType = 0
	bufferDataType dataType = 1
	stringDataType dataType = 2
)

func assemblyScriptTypeOf(memory *wasmedge.Memory, ptr int32) result.Result[dataType] {

	return result.AndThen(
		resultWrap(memory.GetData(uint(ptr)-8, 4)),
		func(dataTypeBytes []byte) result.Result[dataType] {
			// FIXME assuming little endian for now
			DataType := dataType(binary.LittleEndian.Uint32(dataTypeBytes))
			switch DataType {
			case objectDataType:
				// fmt.Fprintln(os.Stderr, "object datatype")
				return result.Ok(objectDataType)
			case bufferDataType:
				// fmt.Fprintln(os.Stderr, "buffer datatype")
				return result.Ok(bufferDataType)
			case stringDataType:
				// fmt.Fprintln(os.Stderr, "string datatype")
				return result.Ok(stringDataType)
			default:
				return result.Err[dataType](fmt.Errorf("unknown assemblyscript data type: rtId=%d", DataType))
			}
		},
	)
}

func parseAllocResult(res []any) result.Result[int32] {
	if len(res) != 1 {
		return result.Err[int32](fmt.Errorf("invalid result when allocating memory for string"))
	}
	ptr, ok := res[0].(int32)
	if !ok {
		return result.Err[int32](fmt.Errorf("invalid result type when allocating memory for string"))
	}
	return result.Ok(ptr)
}

func goAllocString(vm *wasmedge.VM, memory *wasmedge.Memory, t string) result.Result[int32] {
	// mod := vm.GetRegisteredModule("contract")
	b := []byte(t)
	// fmt.Fprintln(os.Stderr, "alloc string", t)
	return result.AndThen(
		result.AndThen(
			resultJoin(
				resultWrap(vm.ExecuteRegistered("contract", "alloc", int32(2*4))),
				resultWrap(vm.ExecuteRegistered("contract", "alloc", int32(len(b)))),
			),
			func(res [][]any) result.Result[[]int32] {
				// fmt.Fprintln(os.Stderr, "allocated string", t)
				return resultJoin(
					parseAllocResult(res[0]),
					parseAllocResult(res[1]),
				)
			},
		),
		func(res []int32) result.Result[int32] {
			// fmt.Fprintln(os.Stderr, "parsed alloc string res", t)
			doublePtr := res[0]
			ptr := res[1]
			if memory == nil {
				// mod := vm.GetRegisteredModule("env")
				// memory = mod.FindMemory("memory")
				mod := vm.GetRegisteredModule("contract")
				memory = mod.FindMemory(mod.ListMemory()[0])
			}
			return result.Map(
				resultJoin(
					result.Map(
						resultWrap(memory.GetData(uint(ptr), uint(len(b)))),
						func(data []byte) int32 {
							// fmt.Fprintln(os.Stderr, "set string data", t)
							copy(data, b)
							return ptr
						},
					),
					result.Map(
						resultWrap(memory.GetData(uint(doublePtr), uint(8))),
						func(data []byte) int32 {
							// fmt.Fprintln(os.Stderr, "set string pointers", t)
							// FIXME assuming little endian for now
							binary.LittleEndian.PutUint32(data[:4], uint32(ptr))
							binary.LittleEndian.PutUint32(data[4:], uint32(len(t)))
							return doublePtr
						},
					),
				),
				func(res []int32) int32 {
					// fmt.Fprintln(os.Stderr, "alloc string complete", t)
					return res[1]
				},
			)
		},
	)
}

func goReadString(memory *wasmedge.Memory, ptr int32) result.Result[string] {
	return result.Map(
		result.AndThen(
			resultWrap(memory.GetData(uint(ptr), 8)),
			func(ptrAndsizeBytes []byte) result.Result[[]byte] {
				ptrBytes := ptrAndsizeBytes[:4]
				sizeBytes := ptrAndsizeBytes[4:]

				// FIXME assuming little endian for now
				size := binary.LittleEndian.Uint32(sizeBytes)
				ptr := binary.LittleEndian.Uint32(ptrBytes)

				memoryBytes, err := memory.GetData(uint(ptr), uint(size))

				return resultWrap(memoryBytes, err)
			},
		),
		func(b []byte) string {
			return string(b)
		},
	)
}

func decodeUtf16(b []byte, order binary.ByteOrder) (string, error) {
	ints := make([]uint16, len(b)/2)
	if err := binary.Read(bytes.NewReader(b), order, &ints); err != nil {
		return "", err
	}
	return string(utf16.Decode(ints)), nil
}

func encodeUtf16(b string, order binary.ByteOrder) ([]byte, error) {
	ints := utf16.Encode([]rune(b))
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, order, &ints); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func readString(runtime wasm_runtime.Runtime, memory *wasmedge.Memory, ptr int32) result.Result[string] {
	return wasm_runtime.Execute(runtime, wasm_runtime.RuntimeAction[result.Result[string]]{
		AssemblyScript: func() result.Result[string] {
			return assemblyScriptReadString(memory, ptr)
		},
		Go: func() result.Result[string] {
			return goReadString(memory, ptr)
		},
	})
}

func allocString(runtime wasm_runtime.Runtime, vm *wasmedge.VM, memory *wasmedge.Memory, t string) result.Result[int32] {
	return wasm_runtime.Execute(runtime, wasm_runtime.RuntimeAction[result.Result[int32]]{
		AssemblyScript: func() result.Result[int32] {
			return assemblyScriptAllocString(vm, memory, t)
		},
		Go: func() result.Result[int32] {
			return goAllocString(vm, memory, t)
		},
	})
}

func registerImport(runtime wasm_runtime.Runtime, vm *wasmedge.VM, gas *uint, client ipc_client.Client[WasmResultStruct], modname string, funcs []sdkTypes.SdkType) result.Result[*wasmedge.Module] {
	mod := wasmedge.NewModule(modname)
	for _, f := range funcs {
		fnType := wasmedge.NewFunctionType(f.Type.Parameters, f.Type.Result)
		defer fnType.Release()
		fn := wasmedge.NewFunction(
			fnType,
			func(data interface{}, callframe *wasmedge.CallingFrame, params []interface{}) ([]interface{}, wasmedge.Result) {
				// fmt.Fprintln(os.Stderr, "fn name:", f.Name)
				memory := callframe.GetMemoryByIndex(0)

				if f.Name == "abort" {
					// abort(msg?: string | null, fileName?: string | null, lineNumber?: i32, columnNumber?: i32)
					msgPtr := params[0].(int32)
					filePtr := params[1].(int32)
					line := params[2].(int32)
					column := params[3].(int32)
					msg := readString(runtime, memory, msgPtr).UnwrapOr("no message")
					file := readString(runtime, memory, filePtr).UnwrapOr("unknown-file")
					errStr := fmt.Sprintf("msg: %s\nfile: %s:%d:%d", msg, file, line, column)
					client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
						Gas: vm.GetStatistics().GetTotalCost(),
					}, Error: &errStr}).Expect("exec finish failed")
					fmt.Fprintln(os.Stderr, client.Close())
					return []any{}, wasmedge.Result_Fail
				}

				parsed := resultJoin(utils.Map(params, func(arg any) result.Result[any] {
					ptr := arg.(int32)

					assemblyScriptTypeOf(memory, ptr).Inspect(func(dt dataType) {
						//fmt.Fprintln(os.Stderr, "data type:", dt)
					}).InspectErr(func(err error) {
						// fmt.Fprintln(os.Stderr, "err:", err)
					})
					// the following assumes that the argument is a string
					return result.Map(
						readString(runtime, memory, ptr),
						func(str string) any {
							return str
						},
					)
				})...)
				if parsed.IsErr() {
					return []any{}, wasmedge.Result_Fail
				}
				args := parsed.Unwrap()

				res := client.Request(&execute.SdkCallRequest[WasmResultStruct]{
					Function: f.Name,
					Argument: args,
				})

				wasmRes, err := resultToWasmEdgeResult(
					runtime,
					vm,
					memory,
					result.Map(
						result.AndThen(
							res,
							func(pm ipc_requests.ProcessedMessage[WasmResultStruct]) WasmResult {
								return resultWrap(pm.Result.Take())
							},
						),
						func(res WasmResultStruct) string {
							*gas -= uint(res.Gas)
							vm.GetStatistics().SetCostLimit(*gas)
							return res.Result
						},
					).InspectErr(func(err error) {
						errStr := err.Error()
						client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
							Gas: vm.GetStatistics().GetTotalCost(),
						}, Error: &errStr}).Expect("exec finish failed")
						fmt.Fprintln(os.Stderr, client.Close())
					}),
				)

				if len(f.Type.Result) == 0 {
					return []any{}, err
				}

				return wasmRes, err
			},
			nil,
			f.Cost,
		)
		mod.AddFunction(f.Name, fn)
	}
	// TODO make this only in "env" module
	if modname == "env" {
		memory := wasmedge.NewMemory(wasmedge.NewMemoryType(wasmedge.NewLimit(1)))
		mod.AddMemory("memory", memory)
	}
	return resultWrap(mod, vm.GetExecutor().RegisterImport(vm.GetStore(), mod)).MapErr(func(err error) error {
		modCleanup(mod)
		return err
	})
}

func registerImportV2(ctx context.Context, runtime wasm_runtime.Runtime, vm *wasmedge.VM, gas *uint, modname string, funcs []sdkTypes.SdkType, retChan chan wasm_types.BasicErrorResult) *wasmedge.Module {
	mod := wasmedge.NewModule(modname)

	for _, f := range funcs {
		fnType := wasmedge.NewFunctionType(f.Type.Parameters, f.Type.Result)
		defer fnType.Release()
		fn := wasmedge.NewFunction(
			fnType,
			func(data interface{}, callframe *wasmedge.CallingFrame, params []interface{}) ([]interface{}, wasmedge.Result) {
				// fmt.Fprintln(os.Stderr, "fn name:", f.Name)
				memory := callframe.GetMemoryByIndex(0)

				if f.Name == "abort" {
					// abort(msg?: string | null, fileName?: string | null, lineNumber?: i32, columnNumber?: i32)
					msgPtr := params[0].(int32)
					filePtr := params[1].(int32)
					line := params[2].(int32)
					column := params[3].(int32)
					msg := readString(runtime, memory, msgPtr).UnwrapOr("no message")
					file := readString(runtime, memory, filePtr).UnwrapOr("unknown-file")
					errStr := fmt.Sprintf("msg: %s\nfile: %s:%d:%d", msg, file, line, column)

					fmt.Println("errStr:", errStr)
					// client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
					// 	Gas: vm.GetStatistics().GetTotalCost(),
					// }, Error: &errStr}).Expect("exec finish failed")

					retChan <- wasm_types.BasicErrorResult{
						Error: &errStr,
						Result: &wasm_types.WasmResultStruct{
							Gas: vm.GetStatistics().GetTotalCost(),
						},
					}

					// fmt.Fprintln(os.Stderr, client.Close())

					fmt.Println("aborting execution:", errStr)
					return []any{}, wasmedge.Result_Fail
				}

				if f.Name == "revert" {
					msgPtr := params[0].(int32)
					symbolPtr := params[1].(int32)

					msg := readString(runtime, memory, msgPtr).UnwrapOr("no message")
					symbol := readString(runtime, memory, symbolPtr).UnwrapOr("no symbol")

					fmt.Println("reverting execution:", msg, symbol)

					retChan <- wasm_types.BasicErrorResult{
						Error: &msg,
					}

					return []any{}, wasmedge.Result_Fail
				}

				parsed := resultJoin(utils.Map(params, func(arg any) result.Result[any] {
					ptr := arg.(int32)

					assemblyScriptTypeOf(memory, ptr).Inspect(func(dt dataType) {
						//fmt.Fprintln(os.Stderr, "data type:", dt)
					}).InspectErr(func(err error) {
						// fmt.Fprintln(os.Stderr, "err:", err)
					})
					// the following assumes that the argument is a string
					return result.Map(
						readString(runtime, memory, ptr),
						func(str string) any {
							return str
						},
					)
				})...)
				if parsed.IsErr() {
					return []any{}, wasmedge.Result_Fail
				}
				args := parsed.Unwrap()

				res := executeImport(ctx, f.Name, args)

				// res := client.Request(&execute.SdkCallRequest[WasmResultStruct]{
				// 	Function: f.Name,
				// 	Argument: args,
				// })

				// fmt.Printf("res %+v\n", res)
				// res.InspectErr(func(err error) {
				// 	fmt.Printf("res.error %s\n", err.Error())
				// })

				// for f.Name == "system.getEnv" {
				// 	break
				// }
				wasmRes, err := resultToWasmEdgeResult(
					runtime,
					vm,
					memory,
					result.Map(
						result.AndThen(
							res,
							func(pm wasm_types.BasicErrorResult) result.Result[wasm_types.WasmResultStruct] {

								if pm.Error != nil {
									fmt.Println("Definite error!", *pm.Error)
								}
								if pm.Error != nil {
									err := errors.New(*pm.Error)

									retChan <- wasm_types.BasicErrorResult{
										Error: pm.Error,
										Result: &wasm_types.WasmResultStruct{
											Gas: vm.GetStatistics().GetTotalCost(),
										},
									}
									return result.Err[wasm_types.WasmResultStruct](err)
								}
								// fmt.Println("Possible error maybe!!", pm.Result)
								return result.Ok(wasm_types.WasmResultStruct{
									Result: pm.Result.Result,
									Gas:    pm.Result.Gas,
									Error:  pm.Error != nil,
								})
							},
						),
						func(res wasm_types.WasmResultStruct) string {
							*gas -= uint(res.Gas)
							vm.GetStatistics().SetCostLimit(*gas)
							return res.Result
						},
					).InspectErr(func(err error) {

						// errStr := err.Error()
						// client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
						// 	Gas: vm.GetStatistics().GetTotalCost(),
						// }, Error: &errStr}).Expect("exec finish failed")
						// fmt.Fprintln(os.Stderr, client.Close())
					}),
				)

				if len(f.Type.Result) == 0 {
					return []any{}, err
				}

				return wasmRes, err
			},
			nil,
			f.Cost,
		)
		mod.AddFunction(f.Name, fn)
	}
	if modname == "env" {
		memory := wasmedge.NewMemory(wasmedge.NewMemoryType(wasmedge.NewLimit(1)))
		mod.AddMemory("memory", memory)
	}

	vm.GetExecutor().RegisterImport(vm.GetStore(), mod)
	return mod
}

func executeImport(ctx context.Context, name string, args []any) result.Result[wasm_types.BasicErrorResult] {
	fn, ok := sdk.SdkModule[name]
	if !ok {
		return result.Err[wasm_types.BasicErrorResult](fmt.Errorf("vm requested non-existing function: %s", name))
	}
	// fmt.Fprintln(os.Stderr, s.Function, s.Argument, fn)
	res := result.MapOrElse(
		reflect.ValueOf(fn).Call(
			append(
				[]reflect.Value{reflect.ValueOf(ctx)},
				utils.Map(
					args,
					func(arg any) reflect.Value {
						return reflect.ValueOf(arg)
					},
				)...,
			),
		)[0].Interface().(sdk.SdkResult),
		func(err error) *wasm_types.BasicErrorResult {
			str := err.Error()
			return &wasm_types.BasicErrorResult{
				Error: &str,
			}
		},
		func(res sdk.SdkResultStruct) *wasm_types.BasicErrorResult {
			return &wasm_types.BasicErrorResult{
				Result: &wasm_types.WasmResultStruct{
					Result: res.Result,
					Gas:    res.Gas,
				},
			}
		},
	)
	return result.Ok(*res)
}

func (w *Wasm) _Execute(gas uint, entrypoint string, args string, runtime wasm_runtime.Runtime) WasmResult {
	client := ipc_client.Run[WasmResultStruct]()
	return w._ExecuteWithClient(gas, entrypoint, args, runtime, client)
}

func (w *Wasm) _ExecuteWithClient(gas uint, entrypoint string, args string, runtime wasm_runtime.Runtime, client result.Result[ipc_client.Client[WasmResultStruct]]) WasmResult {
	conf := wasmedge.NewConfigure()
	defer conf.Release()
	conf.SetStatisticsCostMeasuring(true)
	conf.SetStatisticsInstructionCounting(true)
	conf.SetStatisticsTimeMeasuring(true)

	vm := wasmedge.NewVMWithConfig(conf)
	defer vm.Release()
	type initResult struct {
		mods     []*wasmedge.Module
		byteCode []byte
	}
	return result.Map(
		result.AndThen(
			result.MapOrElse(
				result.AndThen(
					result.AndThen(
						client,
						func(client ipc_client.Client[WasmResultStruct]) result.Result[initResult] {
							return result.AndThen(
								resultJoin(
									registerImport(runtime, vm, &gas, client, "sdk", sdkTypes.SdkTypes),
									registerImport(runtime, vm, &gas, client, "env", []sdkTypes.SdkType{
										{
											Name: "abort",
											Type: sdkTypes.VmType{
												Parameters: []wasmedge.ValType{wasmedge.ValType_I32, wasmedge.ValType_I32, wasmedge.ValType_I32, wasmedge.ValType_I32},
											},
										},
									}),
								),
								func(mods []*wasmedge.Module) result.Result[initResult] {
									return result.AndThen(
										client.Request(&execute.ExecutionReady[WasmResultStruct]{}),
										func(res ipc_requests.ProcessedMessage[WasmResultStruct]) result.Result[initResult] {
											return result.AndThen(
												resultWrap(res.Result.Take()).
													MapErr(func(err error) error {
														return fmt.Errorf("no code provided")
													}),
												func(str WasmResultStruct) result.Result[initResult] {
													return result.Map(
														resultWrap(hex.DecodeString(str.Result)),
														func(byteCode []byte) initResult {
															return initResult{mods, byteCode}
														},
													)
												},
											)
										},
									)
								},
							)
						},
					),
					func(init initResult) result.Result[string] {
						vm.GetStatistics().SetCostLimit(gas)
						res := result.AndThen(
							result.AndThen(
								result.AndThen(
									result.AndThen(
										// result.And(
										result.Err[any](vm.RegisterWasmBuffer("contract", init.byteCode)),
										// result.And(
										// result.Err[any](vm.Validate()),
										// result.Err[any](vm.Instantiate()),
										// ),
										// ),
										func(any) result.Result[[]any] {
											return wasm_runtime.Execute(runtime, wasm_runtime.RuntimeAction[result.Result[[]any]]{
												Go: func() result.Result[[]any] {
													// fmt.Fprintln(os.Stderr, "go runtime init")
													return resultWrap(vm.ExecuteRegistered("contract", "_initialize"))
												},
											})
										},
									),
									func([]any) result.Result[int32] {
										// fmt.Fprintln(os.Stderr, "alloc string")
										// mod := vm.GetRegisteredModule("contract")
										// mod.AddMemory("memory", memory)
										return allocString(runtime, vm, nil, args)
									},
								),
								func(args int32) result.Result[[]any] {
									// fmt.Fprintln(os.Stderr, "execute")
									return resultWrap(vm.ExecuteRegistered("contract", entrypoint, args))
								},
							),
							func(res []any) result.Result[string] {
								// fmt.Printf("complete: %+v\n", res)
								if len(res) != 1 {
									return result.Err[string](fmt.Errorf("not exactly 1 return value"))
								}
								switch v := res[0].(type) {
								case int32:
									mod := vm.GetRegisteredModule("contract")
									memoryList := mod.ListMemory()
									if len(memoryList) == 0 {
										mod = vm.GetRegisteredModule("env")
										memoryList = []string{"memory"}
									}
									memory := mod.FindMemory(memoryList[0])
									return readString(runtime, memory, v)
								}
								return result.Err[string](fmt.Errorf("return value is not a string"))
							},
						)
						for _, mod := range init.mods {
							modCleanup(mod)
						}
						return res
					},
				),
				func(err error) result.Result[string] {
					return result.AndThen(
						client,
						func(client ipc_client.Client[WasmResultStruct]) result.Result[string] {
							errStr := err.Error()
							return result.Map(
								client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
									Gas: vm.GetStatistics().GetTotalCost(),
								}, Error: &errStr}),
								func(any) string {
									return errStr
								},
							)
						},
					)
				},
				func(res string) result.Result[string] {
					return result.AndThen(
						client,
						func(client ipc_client.Client[WasmResultStruct]) result.Result[string] {
							return result.Map(
								client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
									Result: res,
									Gas:    vm.GetStatistics().GetTotalCost(),
								}}),
								func(any) string {
									return res
								},
							)
						},
					)
				},
			),
			func(res string) result.Result[string] {
				// stat := vm.GetStatistics()
				// fmt.Fprintf(os.Stderr, "speed: %f instructions per second\n", stat.GetInstrPerSecond())
				// fmt.Fprintln(os.Stderr, "time:", float64(stat.GetInstrCount())/stat.GetInstrPerSecond(), "seconds")
				return result.AndThen(
					client,
					func(client ipc_client.Client[WasmResultStruct]) result.Result[string] {
						return result.Map(
							result.Err[any](client.Close()),
							func(any) string {
								return res
							},
						)
					},
				)
			},
		),
		func(res string) WasmResultStruct {
			return WasmResultStruct{
				Result: res,
				Gas:    vm.GetStatistics().GetTotalCost(),
			}
		},
	)
	// {"Type":"sdk_call_response","Message":{"Result":"test","Error":null}}
}

func (w *Wasm) Execute(ctx context.Context, gas uint, entrypoint string, args string, runtime wasm_runtime.Runtime) wasm_types.BasicErrorResult {
	conf := wasmedge.NewConfigure()
	defer conf.Release()
	conf.SetStatisticsCostMeasuring(true)
	conf.SetStatisticsInstructionCounting(true)
	conf.SetStatisticsTimeMeasuring(true)

	vm := wasmedge.NewVMWithConfig(conf)
	vm.GetStatistics().SetCostLimit(gas)
	defer vm.Release()

	type initResult struct {
		mods     []*wasmedge.Module
		byteCode []byte
	}

	strCode := ctx.Value(wasm_context.WasmExecCodeCtxKey).(string)

	code, _ := hex.DecodeString(strCode)

	fmt.Println("wasm code:", code[:20], "len:", len(code))

	retChan := make(chan wasm_types.BasicErrorResult, 1)

	//Register imports
	mods := make([]*wasmedge.Module, 0)
	mods = append(mods, registerImportV2(ctx, runtime, vm, &gas, "sdk", sdkTypes.SdkTypes, retChan))
	mods = append(mods, registerImportV2(ctx, runtime, vm, &gas, "env", []sdkTypes.SdkType{
		{
			Name: "abort",
			Type: sdkTypes.VmType{
				Parameters: []wasmedge.ValType{wasmedge.ValType_I32, wasmedge.ValType_I32, wasmedge.ValType_I32, wasmedge.ValType_I32},
			},
		},
	}, retChan))

	init := initResult{
		mods:     mods,
		byteCode: code,
	}

	defer func() {
		for _, mod := range init.mods {
			modCleanup(mod)
		}
	}()

	err := vm.RegisterWasmBuffer("contract", init.byteCode)

	if err != nil {
		errStr := fmt.Errorf("failed to register wasm buffer: %w", err).Error()
		return wasm_types.BasicErrorResult{
			Error: &errStr,
			Result: &wasm_types.WasmResultStruct{
				Gas:   vm.GetStatistics().GetTotalCost(),
				Error: true,
			},
		}
	}

	wasm_runtime.Execute(runtime, wasm_runtime.RuntimeAction[result.Result[[]any]]{
		Go: func() result.Result[[]any] {
			// fmt.Fprintln(os.Stderr, "go runtime init")
			return resultWrap(vm.ExecuteRegistered("contract", "_initialize"))
		},
	})

	argsAlloc := allocString(runtime, vm, nil, args)

	argsAlloc.InspectErr(func(err error) {
		errStr := fmt.Errorf("failed to allocate string: %w", err).Error()
		fmt.Println(errStr)
	})
	callResult := resultWrap(vm.ExecuteRegistered("contract", entrypoint, argsAlloc.Unwrap()))

	// errStr := callResult.UnwrapErr()
	// fmt.Println("callErr:", errStr.Error())

	if callResult.IsErr() {
		errStr := callResult.UnwrapErr().Error()
		if len(retChan) > 0 {
			retVal := <-retChan
			fmt.Println(*retVal.Result, *retVal.Error)
			return wasm_types.BasicErrorResult{
				Error: retVal.Error,
				Result: &wasm_types.WasmResultStruct{
					Gas:   vm.GetStatistics().GetTotalCost(),
					Error: true,
				},
			}
		}

		return wasm_types.BasicErrorResult{
			Error: &errStr,
			Result: &wasm_types.WasmResultStruct{
				Gas:   vm.GetStatistics().GetTotalCost(),
				Error: true,
			},
		}
	}
	res := callResult.Unwrap()
	if len(res) != 1 {
		errStr := fmt.Errorf("not exactly 1 return value").Error()
		return wasm_types.BasicErrorResult{
			Error: &errStr,
			Result: &wasm_types.WasmResultStruct{
				Gas:   vm.GetStatistics().GetTotalCost(),
				Error: true,
			},
		}
	}

	var resultStr result.Result[string]
	switch v := res[0].(type) {
	case int32:
		mod := vm.GetRegisteredModule("contract")
		memoryList := mod.ListMemory()
		if len(memoryList) == 0 {
			mod = vm.GetRegisteredModule("env")
			memoryList = []string{"memory"}
		}
		memory := mod.FindMemory(memoryList[0])
		resultStr = readString(runtime, memory, v)
	}

	totalGasCost := vm.GetStatistics().GetTotalCost()
	fmt.Println("total gas cost:", totalGasCost)

	return wasm_types.BasicErrorResult{
		Error: nil,
		Result: &wasm_types.WasmResultStruct{
			Result: resultStr.Unwrap(),
			Gas:    totalGasCost,
			Error:  false,
		},
	}
	// return result.Map(
	// 	result.AndThen(
	// 		result.MapOrElse(
	// 			result.AndThen(
	// 				result.AndThen(
	// 					// client,
	// 					func(client ipc_client.Client[WasmResultStruct]) result.Result[initResult] {
	// 						return result.AndThen(
	// 							resultJoin(
	// 								registerImport(runtime, vm, &gas, client, "sdk", sdkTypes.SdkTypes),
	// 								registerImport(runtime, vm, &gas, client, "env", []sdkTypes.SdkType{
	// 									{
	// 										Name: "abort",
	// 										Type: sdkTypes.VmType{
	// 											Parameters: []wasmedge.ValType{wasmedge.ValType_I32, wasmedge.ValType_I32, wasmedge.ValType_I32, wasmedge.ValType_I32},
	// 										},
	// 									},
	// 								}),
	// 							),
	// 							func(mods []*wasmedge.Module) result.Result[initResult] {
	// 								return result.AndThen(
	// 									client.Request(&execute.ExecutionReady[WasmResultStruct]{}),
	// 									func(res ipc_requests.ProcessedMessage[WasmResultStruct]) result.Result[initResult] {
	// 										return result.AndThen(
	// 											resultWrap(res.Result.Take()).
	// 												MapErr(func(err error) error {
	// 													return fmt.Errorf("no code provided")
	// 												}),
	// 											func(str WasmResultStruct) result.Result[initResult] {
	// 												return result.Map(
	// 													resultWrap(hex.DecodeString(str.Result)),
	// 													func(byteCode []byte) initResult {
	// 														return initResult{mods, byteCode}
	// 													},
	// 												)
	// 											},
	// 										)
	// 									},
	// 								)
	// 							},
	// 						)
	// 					},
	// 				),
	// 				func(init initResult) result.Result[string] {
	// 					vm.GetStatistics().SetCostLimit(gas)
	// 					res := result.AndThen(
	// 						result.AndThen(
	// 							result.AndThen(
	// 								result.AndThen(
	// 									// result.And(
	// 									result.Err[any](vm.RegisterWasmBuffer("contract", init.byteCode)),
	// 									// result.And(
	// 									// result.Err[any](vm.Validate()),
	// 									// result.Err[any](vm.Instantiate()),
	// 									// ),
	// 									// ),
	// 									func(any) result.Result[[]any] {
	// 										return wasm_runtime.Execute(runtime, wasm_runtime.RuntimeAction[result.Result[[]any]]{
	// 											Go: func() result.Result[[]any] {
	// 												// fmt.Fprintln(os.Stderr, "go runtime init")
	// 												return resultWrap(vm.ExecuteRegistered("contract", "_initialize"))
	// 											},
	// 										})
	// 									},
	// 								),
	// 								func([]any) result.Result[int32] {
	// 									// fmt.Fprintln(os.Stderr, "alloc string")
	// 									// mod := vm.GetRegisteredModule("contract")
	// 									// mod.AddMemory("memory", memory)
	// 									return allocString(runtime, vm, nil, args)
	// 								},
	// 							),
	// 							func(args int32) result.Result[[]any] {
	// 								// fmt.Fprintln(os.Stderr, "execute")
	// 								return resultWrap(vm.ExecuteRegistered("contract", entrypoint, args))
	// 							},
	// 						),
	// 						func(res []any) result.Result[string] {
	// 							// fmt.Printf("complete: %+v\n", res)
	// 							if len(res) != 1 {
	// 								return result.Err[string](fmt.Errorf("not exactly 1 return value"))
	// 							}
	// 							switch v := res[0].(type) {
	// 							case int32:
	// 								mod := vm.GetRegisteredModule("contract")
	// 								memoryList := mod.ListMemory()
	// 								if len(memoryList) == 0 {
	// 									mod = vm.GetRegisteredModule("env")
	// 									memoryList = []string{"memory"}
	// 								}
	// 								memory := mod.FindMemory(memoryList[0])
	// 								return readString(runtime, memory, v)
	// 							}
	// 							return result.Err[string](fmt.Errorf("return value is not a string"))
	// 						},
	// 					)
	// 					for _, mod := range init.mods {
	// 						modCleanup(mod)
	// 					}
	// 					return res
	// 				},
	// 			),
	// 			func(err error) result.Result[string] {
	// 				return result.AndThen(
	// 					client,
	// 					func(client ipc_client.Client[WasmResultStruct]) result.Result[string] {
	// 						errStr := err.Error()
	// 						return result.Map(
	// 							client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
	// 								Gas: vm.GetStatistics().GetTotalCost(),
	// 							}, Error: &errStr}),
	// 							func(any) string {
	// 								return errStr
	// 							},
	// 						)
	// 					},
	// 				)
	// 			},
	// 			func(res string) result.Result[string] {
	// 				return result.AndThen(
	// 					client,
	// 					func(client ipc_client.Client[WasmResultStruct]) result.Result[string] {
	// 						return result.Map(
	// 							client.Send(&execute.ExecutionFinish[WasmResultStruct]{Result: &wasm_types.WasmResultStruct{
	// 								Result: res,
	// 								Gas:    vm.GetStatistics().GetTotalCost(),
	// 							}}),
	// 							func(any) string {
	// 								return res
	// 							},
	// 						)
	// 					},
	// 				)
	// 			},
	// 		),
	// 		func(res string) result.Result[string] {
	// 			// stat := vm.GetStatistics()
	// 			// fmt.Fprintf(os.Stderr, "speed: %f instructions per second\n", stat.GetInstrPerSecond())
	// 			// fmt.Fprintln(os.Stderr, "time:", float64(stat.GetInstrCount())/stat.GetInstrPerSecond(), "seconds")
	// 			return result.AndThen(
	// 				client,
	// 				func(client ipc_client.Client[WasmResultStruct]) result.Result[string] {
	// 					return result.Map(
	// 						result.Err[any](client.Close()),
	// 						func(any) string {
	// 							return res
	// 						},
	// 					)
	// 				},
	// 			)
	// 		},
	// 	),
	// 	func(res string) WasmResultStruct {
	// 		return WasmResultStruct{
	// 			Result: res,
	// 			Gas:    vm.GetStatistics().GetTotalCost(),
	// 		}
	// 	},
	// )
}

var ErrResultTypeCast = fmt.Errorf("type cast")

func resultWrapTypeCast[T any](val any) result.Result[T] {
	res, ok := val.(T)
	if !ok {
		return result.Err[T](ErrResultTypeCast)
	}
	return result.Ok(res)
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}

func resultJoin[T any](results ...result.Result[T]) (res result.Result[[]T]) {
	for _, r := range results {
		if r.IsOk() {
			res = result.Ok(append(res.Unwrap(), r.Unwrap()))
		} else {
			return result.Map(r, func(T) []T {
				return nil
			})
		}
	}
	return res
}
