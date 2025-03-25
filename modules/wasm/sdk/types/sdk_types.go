package sdk_types

import (
	"reflect"
	"vsc-node/modules/wasm/sdk"

	"github.com/second-state/WasmEdge-go/wasmedge"
)

var SdkTypes = generateSdkTypes()

type SdkType struct {
	Name string
	Type VmType
	Cost uint
}

type VmType struct {
	Parameters []wasmedge.ValType
	Result     []wasmedge.ValType
}

func generateSdkTypes() []SdkType {
	res := make([]SdkType, 0)
	for name, fn := range sdk.SdkModule {
		t := reflect.TypeOf(fn)
		res = append(res, SdkType{
			name,
			VmType{
				generateFunctionParameters(t),
				generateFunctionResult(t),
			},
			generateCost(name, t),
		})
	}
	return res
}

// TODO: do automatic source code analysis on sdk.SdkModule to generate a cost map
func generateCost(name string, fn reflect.Type) uint {
	return 0
}

func generateFunctionResult(fn reflect.Type) []wasmedge.ValType {
	c := fn.NumOut()
	res := make([]wasmedge.ValType, c)

	for i := range res {
		res[i] = goTypeToWasmType(fn.Out(i))
	}

	return res
}

func generateFunctionParameters(fn reflect.Type) []wasmedge.ValType {
	c := fn.NumIn()
	res := make([]wasmedge.ValType, c-1)

	for i := range res {
		res[i] = goTypeToWasmType(fn.In(i + 1))
	}

	return res
}

// all go types map to wasm 32-bit pointers by default
func goTypeToWasmType(reflect.Type) wasmedge.ValType {
	return wasmedge.ValType_I32
}
