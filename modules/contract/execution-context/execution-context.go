package contract_execution_context

import wasm_context "vsc-node/modules/wasm/context"

type contractExecutionContext struct {
	contractId string
}

type ContractExecutionContext = *contractExecutionContext

var _ wasm_context.ExecContextValue = &contractExecutionContext{}

func New(contractId string) ContractExecutionContext {
	return &contractExecutionContext{
		contractId: contractId,
	}
}
