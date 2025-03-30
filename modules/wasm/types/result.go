package wasm_types

import "github.com/JustinKnueppel/go-result"

type WasmResultStruct struct {
	Result string
	Gas    uint
	Error  bool
}

type WasmResult = result.Result[WasmResultStruct]
