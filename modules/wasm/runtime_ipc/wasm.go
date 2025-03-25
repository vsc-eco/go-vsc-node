package wasm_runtime_ipc

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"unicode/utf16"
	ipc_client "vsc-node/lib/stdio-ipc/client"
	"vsc-node/lib/utils"
	a "vsc-node/modules/aggregate"
	"vsc-node/modules/wasm/ipc_requests"
	"vsc-node/modules/wasm/ipc_requests/execute"
	wasm_runtime "vsc-node/modules/wasm/runtime"
	sdkTypes "vsc-node/modules/wasm/sdk/types"

	"github.com/JustinKnueppel/go-result"
	"github.com/chebyrash/promise"
	"github.com/second-state/WasmEdge-go/wasmedge"
	// bindgen "github.com/second-state/wasmedge-bindgen/host/go"
)

type Wasm struct {
}

var _ a.Plugin = &Wasm{}

func New() *Wasm {
	return &Wasm{}
}

func (w *Wasm) Init() error {
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
			return resultWrap(decodeUtf16(b, binary.LittleEndian))
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
	fmt.Fprintln(os.Stderr, "alloc string", t)
	return result.AndThen(
		result.AndThen(
			resultJoin(
				resultWrap(vm.ExecuteRegistered("contract", "alloc", int32(2*4))),
				resultWrap(vm.ExecuteRegistered("contract", "alloc", int32(len(b)))),
			),
			func(res [][]any) result.Result[[]int32] {
				fmt.Fprintln(os.Stderr, "allocated string", t)
				return resultJoin(
					parseAllocResult(res[0]),
					parseAllocResult(res[1]),
				)
			},
		),
		func(res []int32) result.Result[int32] {
			fmt.Fprintln(os.Stderr, "parsed alloc string res", t)
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
							fmt.Fprintln(os.Stderr, "set string data", t)
							copy(data, b)
							return ptr
						},
					),
					result.Map(
						resultWrap(memory.GetData(uint(doublePtr), uint(8))),
						func(data []byte) int32 {
							fmt.Fprintln(os.Stderr, "set string pointers", t)
							// FIXME assuming little endian for now
							binary.LittleEndian.PutUint32(data[:4], uint32(ptr))
							binary.LittleEndian.PutUint32(data[4:], uint32(len(t)))
							return doublePtr
						},
					),
				),
				func(res []int32) int32 {
					fmt.Fprintln(os.Stderr, "alloc string complete", t)
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

				return resultWrap(memory.GetData(uint(ptr), uint(size)))
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

func registerImport(runtime wasm_runtime.Runtime, vm *wasmedge.VM, client ipc_client.Client[string], modname string, funcs []sdkTypes.SdkType) result.Result[*wasmedge.Module] {
	mod := wasmedge.NewModule(modname)
	for _, f := range funcs {
		fnType := wasmedge.NewFunctionType(f.Type.Parameters, f.Type.Result)
		defer fnType.Release()
		fn := wasmedge.NewFunction(
			fnType,
			func(data interface{}, callframe *wasmedge.CallingFrame, params []interface{}) ([]interface{}, wasmedge.Result) {
				var arg string
				memory := callframe.GetMemoryByIndex(0)
				if len(params) == 1 {
					ptr := params[0].(int32)
					// the following assumes that the argument is a string
					res := readString(runtime, memory, ptr)
					if res.IsErr() {
						return []any{}, wasmedge.Result_Fail
					}
					arg = res.Unwrap()
				} else if len(params) > 1 {
					return []any{}, wasmedge.Result_Fail
				}

				res := client.Request(&execute.SdkCallRequest[string]{
					Function: f.Name,
					Argument: arg,
				})

				// fmt.Printf("res %+v\n", res)
				// res.InspectErr(func(err error) {
				// 	fmt.Printf("res.error %s\n", err.Error())
				// })

				return resultToWasmEdgeResult(
					runtime,
					vm,
					memory,
					result.AndThen(
						res,
						func(pm ipc_requests.ProcessedMessage[string]) result.Result[string] {
							return resultWrap(pm.Result.Take())
						},
					),
				)
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

func (w *Wasm) Execute(gas uint, entrypoint string, args string, runtime wasm_runtime.Runtime) result.Result[string] {
	client := ipc_client.Run[string]()
	return w.ExecuteWithClient(gas, entrypoint, args, runtime, client)
}

func (w *Wasm) ExecuteWithClient(gas uint, entrypoint string, args string, runtime wasm_runtime.Runtime, client result.Result[ipc_client.Client[string]]) result.Result[string] {
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
	return result.AndThen(
		result.MapOrElse(
			result.AndThen(
				result.AndThen(
					client,
					func(client ipc_client.Client[string]) result.Result[initResult] {
						return result.AndThen(
							resultJoin(
								registerImport(runtime, vm, client, "sdk", sdkTypes.SdkTypes),
								registerImport(runtime, vm, client, "env", []sdkTypes.SdkType{
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
									client.Request(&execute.ExecutionReady[string]{}),
									func(res ipc_requests.ProcessedMessage[string]) result.Result[initResult] {
										return result.AndThen(
											resultWrap(res.Result.Take()).
												MapErr(func(err error) error {
													return fmt.Errorf("no code provided")
												}),
											func(str string) result.Result[initResult] {
												return result.Map(
													resultWrap(hex.DecodeString(str)),
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
												fmt.Fprintln(os.Stderr, "go runtime init")
												return resultWrap(vm.ExecuteRegistered("contract", "_initialize"))
											},
										})
									},
								),
								func([]any) result.Result[int32] {
									fmt.Fprintln(os.Stderr, "alloc string")
									// mod := vm.GetRegisteredModule("contract")
									// mod.AddMemory("memory", memory)
									return allocString(runtime, vm, nil, args)
								},
							),
							func(args int32) result.Result[[]any] {
								fmt.Fprintln(os.Stderr, "execute")
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
					func(client ipc_client.Client[string]) result.Result[string] {
						errStr := err.Error()
						return result.Map(
							client.Send(&execute.ExecutionFinish[string]{Error: &errStr}),
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
					func(client ipc_client.Client[string]) result.Result[string] {
						return result.Map(
							client.Send(&execute.ExecutionFinish[string]{Result: &res}),
							func(any) string {
								return res
							},
						)
					},
				)
			},
		),
		func(res string) result.Result[string] {
			stat := vm.GetStatistics()
			fmt.Fprintln(os.Stderr, "time:", float64(stat.GetInstrCount())/stat.GetInstrPerSecond(), "seconds")
			return result.AndThen(
				client,
				func(client ipc_client.Client[string]) result.Result[string] {
					return result.Map(
						result.Err[any](client.Close()),
						func(any) string {
							return res
						},
					)
				},
			)
		},
	)
	// {"Type":"sdk_call_response","Message":{"Result":"test","Error":null}}
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
