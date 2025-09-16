package wasm_types

import (
	"vsc-node/modules/db/vsc/contracts"

	"github.com/JustinKnueppel/go-result"
)

type WasmResultStruct struct {
	Result    string
	Gas       uint
	Error     *string
	ErrorCode contracts.ContractOutputError
}

type WasmResult = result.Result[WasmResultStruct]
