package wasm_context

type contextKey string

const WasmExecCtxKey = contextKey("exec")

type ExecContextValue any

const WasmExecCodeCtxKey = contextKey("exec-code")
