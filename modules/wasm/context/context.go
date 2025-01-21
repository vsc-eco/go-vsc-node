package wasm_context

type contextKey string

const WasmExecCtxKey = contextKey("exec")

type ExecContextValue any
