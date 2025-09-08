package wasm_types

import "github.com/JustinKnueppel/go-result"

type WasmResultStruct struct {
	Result string
	Gas    uint
	Error  bool
}

type WasmResult = result.Result[WasmResultStruct]

type BasicErrorResult struct {
	Result    *WasmResultStruct
	Error     *string
	ErrorCode ErrorSymbol
}

// MISC: @TODO cleanup
type ErrorSymbol string

const (
	REVERT ErrorSymbol = "revert"
)
