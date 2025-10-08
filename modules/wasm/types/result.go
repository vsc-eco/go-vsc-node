package wasm_types

import (
	"vsc-node/modules/db/vsc/contracts"

	"github.com/JustinKnueppel/go-result"
)

type WasmResultStruct struct {
	Result string
	Gas    uint
	Error  bool
}

type WasmResult = result.Result[WasmResultStruct]

type BasicErrorResult struct {
	Result    *WasmResultStruct
	Error     *string
	ErrorCode contracts.ContractOutputError
}
