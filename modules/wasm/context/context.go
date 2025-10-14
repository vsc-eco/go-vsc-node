package wasm_context

import (
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
)

type contextKey string

const WasmExecCtxKey = contextKey("exec")
const WasmExecCodeCtxKey = contextKey("exec-code")

type IOSession interface {
	End() uint
}

type ExecContextValue interface {
	ContractCall(contractId string, method string, payload string, options string) wasm_types.WasmResult
	ContractStateGet(contractId string, key string) result.Result[string]
	DeleteState(key string) result.Result[struct{}]
	EnvVar(key string) result.Result[string]
	GetBalance(account string, asset string) int64
	GetEnv() result.Result[string]
	GetState(key string) result.Result[string]
	IOGas() int
	IOSession() IOSession
	Log(msg string)
	PullBalance(amount int64, asset string) result.Result[struct{}]
	Revert()
	SendBalance(to string, amount int64, asset string) result.Result[struct{}]
	SetGasUsage(gasUsed uint)
	SetState(key string, value string) result.Result[struct{}]
	WithdrawBalance(to string, amount int64, asset string) result.Result[struct{}]
	TssCreateKey(keyId string, keyType string) result.Result[string]
	TssGetKey(keyId string) result.Result[string]
	TssKeySign(keyId string, msg string) result.Result[string]
}
