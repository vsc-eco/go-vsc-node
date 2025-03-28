package contract_execution_context

import (
	"fmt"
	"slices"
	"vsc-node/modules/db/vsc/contracts"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/JustinKnueppel/go-result"
)

type contractExecutionContext struct {
	contractId string
	intents    []contracts.Intent
	ledger     ledgerSystem.LedgerSession

	logs []string

	ioGas      int
	ioSessions []IOSession

	env map[string]string
}

type ContractExecutionContext = *contractExecutionContext

// var _ wasm_context.ExecContextValue = &contractExecutionContext{}

func New(contractId string, intents []contracts.Intent, ledger ledgerSystem.LedgerSession) ContractExecutionContext {
	return &contractExecutionContext{
		contractId: contractId,
		intents:    intents,
		ledger:     ledger,
	}
}

func (ctx *contractExecutionContext) IOGas() int {
	return ctx.ioGas
}

type ioSession struct {
	ioGas int
	end   func() int
}

type IOSession = *ioSession

func (ctx *contractExecutionContext) IOSession() IOSession {
	session := &ioSession{}
	session.end = func() int {
		i := slices.Index(ctx.ioSessions, session)
		ctx.ioSessions[i] = nil
		return session.ioGas
	}

	i := slices.Index(ctx.ioSessions, nil)
	if i == -1 {
		ctx.ioSessions = append(ctx.ioSessions, session)
	} else {
		ctx.ioSessions[i] = session
	}

	return session
}

func (session *ioSession) End() uint {
	return uint(session.end())
}

func (ctx *contractExecutionContext) doIO(gas int) {
	for _, session := range ctx.ioSessions {
		session.ioGas += gas
	}
	ctx.ioGas += gas
}

func (ctx *contractExecutionContext) Revert() {
	// ctx.ledger.Revert()
}

func (ctx *contractExecutionContext) Log(msg string) {
	ctx.logs = append(ctx.logs, msg)
	ctx.doIO(len(msg))
}

func (ctx *contractExecutionContext) EnvVar(key string) result.Result[string] {
	val, ok := ctx.env[key]
	if ok {
		return result.Ok(val)
	}
	return result.Err[string](fmt.Errorf("environemnt does not contain value for \"%s\"", key))
}

func (ctx *contractExecutionContext) SetState(key string, value string) result.Result[struct{}] {
	ctx.doIO(len(key) + len(value))
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) GetState(key string) result.Result[string] {
	ctx.doIO(len(key))
	return result.Ok("TODO")
}

func (ctx *contractExecutionContext) DeleteState(key string) result.Result[struct{}] {
	ctx.doIO(len(key))
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) DrawFunds() {
	// ctx.ledger.
}
