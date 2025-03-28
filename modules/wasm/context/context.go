package wasm_context

import contract_execution_context "vsc-node/modules/contract/execution-context"

type contextKey string

const WasmExecCtxKey = contextKey("exec")

type ExecContextValue = contract_execution_context.ContractExecutionContext

const WasmExecCodeCtxKey = contextKey("exec-code")
