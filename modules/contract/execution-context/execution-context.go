package contract_execution_context

import (
	"encoding/json"
	"fmt"
	"slices"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/contracts"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/JustinKnueppel/go-result"
)

type contractExecutionContext struct {
	intents []contracts.Intent
	ledger  ledgerSystem.LedgerSession
	env     Environment

	logs []string

	ioReadGas  int
	ioWriteGas int
	ioSessions []IOSession
}

type ContractExecutionContext = *contractExecutionContext

type Environment struct {
	ContractId           string
	BlockHeight          uint64
	TxId                 string
	BlockId              string
	Index                int
	OpIndex              int
	Timestamp            string
	RequiredAuths        []string
	RequiredPostingAuths []string
}

// var _ wasm_context.ExecContextValue = &contractExecutionContext{}

func New(env Environment, intents []contracts.Intent, ledger ledgerSystem.LedgerSession) ContractExecutionContext {
	return &contractExecutionContext{
		intents,
		ledger,
		env,
		nil,
		0,
		0,
		nil,
	}
}

func (ctx *contractExecutionContext) IOGas() int {
	return (ctx.ioReadGas*common.READ_IO_GAS_RC_COST + ctx.ioWriteGas*common.WRITE_IO_GAS_RC_COST) * common.CYCLE_GAS_PER_RC
}

type ioSession struct {
	ioReadGas  int
	ioWriteGas int
	end        func() int
}

type IOSession = *ioSession

func (ctx *contractExecutionContext) IOSession() IOSession {
	session := &ioSession{}
	session.end = func() int {
		i := slices.Index(ctx.ioSessions, session)
		ctx.ioSessions[i] = nil
		return (session.ioReadGas*common.READ_IO_GAS_RC_COST + session.ioWriteGas*common.WRITE_IO_GAS_RC_COST) * common.CYCLE_GAS_PER_RC
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

func (ctx *contractExecutionContext) doIO(readGas int, WriteGas ...int) {
	writeGas := 0
	if len(WriteGas) > 0 {
		writeGas = WriteGas[0]
	}
	for _, session := range ctx.ioSessions {
		session.ioReadGas += readGas
		session.ioWriteGas += writeGas
	}
	ctx.ioReadGas += readGas
	ctx.ioWriteGas += writeGas
}

func (ctx *contractExecutionContext) Revert() {
	ctx.ledger.Revert()
}

func (ctx *contractExecutionContext) Log(msg string) {
	ctx.logs = append(ctx.logs, msg)
	ctx.doIO(len(msg))
}

func (ctx *contractExecutionContext) EnvVar(key string) result.Result[string] {
	switch key {
	case "contract_id":
		return result.Ok(ctx.env.ContractId)
	case "msg.required_auths":
		return result.Map(
			resultWrap(json.Marshal(ctx.env.RequiredAuths)),
			func(b []byte) string {
				return string(b)
			},
		)
	case "msg.required_posting_auths":
		return result.Map(
			resultWrap(json.Marshal(ctx.env.RequiredPostingAuths)),
			func(b []byte) string {
				return string(b)
			},
		)
	case "anchor.id": // tx ID
		return result.Ok(ctx.env.TxId)
	case "anchor.block": // block ID
		return result.Ok(ctx.env.BlockId)
	case "anchor.height":
		return result.Ok(fmt.Sprint(ctx.env.BlockHeight))
	case "anchor.timestamp":
		return result.Ok(ctx.env.Timestamp)
	case "anchor.tx_index":
		return result.Ok(fmt.Sprint(ctx.env.Index))
	case "anchor.op_index":
		return result.Ok(fmt.Sprint(ctx.env.OpIndex))
	case "anchor.included_index":
		return result.Ok("0") // TODO add for VSC txs
	}
	return result.Err[string](fmt.Errorf("environemnt does not contain value for \"%s\"", key))
}

func (ctx *contractExecutionContext) SetState(key string, value string) result.Result[struct{}] {
	// TODO replace logic with current & max size counter
	result.MapOrElse(
		ctx.GetState(key),
		func(err error) any { // key didn't exist before
			// new write key & value
			// no modifications
			ctx.doIO(0, len(key)+len(value))
			return nil
		},
		func(prevValue string) any {
			if len(prevValue) < len(value) { // previous value is shorter than new value
				// new write value excluding previously written bytes
				// modification previous value
				ctx.doIO(len(prevValue), len(value)-len(prevValue))
			} else {
				// no new write
				// modification value
				ctx.doIO(len(value))
			}
			return nil
		},
	)
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

func (ctx *contractExecutionContext) GetBalance(account string, asset string) int64 {
	return ctx.ledger.GetBalance(account, 0, asset) // TODO get block height from env
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}
