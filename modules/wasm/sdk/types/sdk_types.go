package sdk_types

import (
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
		res = append(res, SdkType{
			name,
			VmType{
				generateFunctionParameters(fn),
				generateFunctionResult(fn),
			},
			generateCost(name, fn),
		})
	}
	return res
}

func generateCost(name string, fn any) uint {
	return 0
}

func generateFunctionResult(fn any) []wasmedge.ValType {
	return []wasmedge.ValType{
		wasmedge.ValType_I32,
	}
}

func generateFunctionParameters(fn any) []wasmedge.ValType {
	return []wasmedge.ValType{
		wasmedge.ValType_I32,
	}
}
