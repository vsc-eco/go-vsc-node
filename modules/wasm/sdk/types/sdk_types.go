package sdk_types

import (
	"fmt"
	"os"
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
				generateFunctionParameters(name, t),
				generateFunctionResult(name, t),
			},
			generateCost(name, t),
		})
	}
	fmt.Fprintln(os.Stderr, "sdk types", res)
	return res
}

// TODO: do automatic source code analysis on sdk.SdkModule to generate a cost map
func generateCost(name string, fn reflect.Type) uint {
	return 0
}

func generateFunctionResult(name string, fn reflect.Type) []wasmedge.ValType {
	// functions without a return
	switch name {
	case "console.log":
		fallthrough
	case "db.setObject":
		fallthrough
	case "db.delObject":
		fallthrough
	case "hive.draw":
		fallthrough
	case "hive.transfer":
		fallthrough
	case "hive.withdraw":
		fallthrough
	case "ic.link":
		fallthrough
	case "ic.unlink":
		return nil
	default:
		// generate result type
	}
	c := fn.NumOut()
	res := make([]wasmedge.ValType, c)

	for i := range res {
		res[i] = goTypeToWasmType(fn.Out(i))
	}

	return res
}

func generateFunctionParameters(name string, fn reflect.Type) []wasmedge.ValType {
	c := fn.NumIn()
	res := make([]wasmedge.ValType, c-1)

	for i := range res {
		if name == "console.logNumber" {
			res[i] = wasmedge.ValType_F64
			continue
		}
		res[i] = goTypeToWasmType(fn.In(i + 1))
	}

	return res
}

// all go types map to wasm 32-bit pointers by default
func goTypeToWasmType(reflect.Type) wasmedge.ValType {
	return wasmedge.ValType_I32
}
