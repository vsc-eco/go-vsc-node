package contract_execution_context

import (
	"encoding/json"
	"fmt"
	"slices"
	"vsc-node/modules/common"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/contracts"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/JustinKnueppel/go-result"
)

type contractExecutionContext struct {
	intents     []contracts.Intent
	ledger      ledgerSystem.LedgerSession
	env         Environment
	rcLimit     int64
	storage     *contract_session.StateStore
	currentSize int
	maxSize     int

	logs []string

	ioReadGas  int
	ioWriteGas int
	ioSessions []IOSession

	tokenLimits map[string]*int64
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

func New(env Environment, rcLimit int64, intents []contracts.Intent, ledger ledgerSystem.LedgerSession, storage *contract_session.StateStore, interalStorage map[string]interface{}) ContractExecutionContext {
	seenTypes := make(map[string]bool)
	tokenLimits := make(map[string]*int64)
	for _, intent := range intents {
		if intent.Type == "transfer.allow" {
			limit, ok := intent.Args["limit"]
			if !ok {
				continue
			}
			token, ok := intent.Args["token"]
			if !ok {
				continue
			}
			key := intent.Type + "-" + token
			seen, _ := seenTypes[key]
			if seen {
				continue
			}
			seenTypes[key] = true
			val, _ := common.SafeParseHiveFloat(limit)
			tokenLimits[token] = &val
		}
	}
	ensureInternalStorageInitialized(interalStorage)
	return &contractExecutionContext{
		intents,
		ledger,
		env,
		rcLimit,
		storage,
		interalStorage["current_size"].(int),
		interalStorage["max_size"].(int),
		nil,
		0,
		0,
		nil,
		tokenLimits,
	}
}

func ensureInternalStorageInitialized(internalStorage map[string]interface{}) {
	_, ok := internalStorage["current_size"]
	if !ok {
		internalStorage["current_size"] = 0
	}
	_, ok = internalStorage["max_size"]
	if !ok {
		internalStorage["max_size"] = 0
	}
}

func (ctx *contractExecutionContext) InternalStorage() map[string]interface{} {
	return map[string]interface{}{
		"current_size": ctx.currentSize,
		"max_size":     ctx.maxSize,
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
	case "tx.origin":
		fallthrough
	case "msg.sender":
		if len(ctx.env.RequiredAuths) > 0 {
			return result.Ok(ctx.env.RequiredAuths[0])
		}
		if len(ctx.env.RequiredPostingAuths) > 0 {
			return result.Ok(ctx.env.RequiredPostingAuths[0])
		}
		return result.Err[string](fmt.Errorf("no value for %s", key))
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
	newWriteToAdd := 0
	result.MapOrElse(
		ctx.GetState(key),
		func(err error) any { // key didn't exist before
			// new write key & value
			// no modifications
			newWriteToAdd = len(key) + len(value)
			return nil
		},
		func(prevValue string) any {
			if len(prevValue) < len(value) { // previous value is shorter than new value
				// new write value excluding previously written bytes
				// modification previous value
				newWriteToAdd = len(value) - len(prevValue)
				ctx.doIO(len(prevValue))
			} else {
				// no new write
				// modification value
				ctx.doIO(len(value))
			}
			return nil
		},
	)
	ctx.currentSize += newWriteToAdd
	newWriteGas := max(0, ctx.currentSize-ctx.maxSize)
	ctx.maxSize = max(ctx.currentSize, ctx.maxSize)
	ctx.doIO(newWriteToAdd-newWriteGas, newWriteGas)
	ctx.storage.Set(key, []byte(value))
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) GetState(key string) result.Result[string] {
	ctx.doIO(len(key))
	res := ctx.storage.Get(key)
	if res == nil {
		return result.Err[string](fmt.Errorf("key does not exist"))
	}
	ctx.doIO(len(string(res)))
	return result.Ok(string(res))
}

func (ctx *contractExecutionContext) DeleteState(key string) result.Result[struct{}] {
	ctx.doIO(len(key))
	value := ctx.GetState(key).Unwrap()
	ctx.currentSize -= len(value) + len(key)
	ctx.storage.Delete(key)
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) GetBalance(account string, asset string) int64 {
	return ctx.ledger.GetBalance(account, ctx.env.BlockHeight, asset)
}

func (ctx *contractExecutionContext) PullBalance(amount int64, asset string) result.Result[struct{}] {
	if len(ctx.env.RequiredAuths) == 0 {
		return result.Err[struct{}](fmt.Errorf("no active authority"))
	}
	tokenLimit, ok := ctx.tokenLimits[asset]
	if !ok {
		return result.Err[struct{}](fmt.Errorf("no user intent for: %s", asset))
	}
	if amount > *tokenLimit {
		return result.Err[struct{}](fmt.Errorf("amount (%d) is over remaining token limit (%d)", amount, *tokenLimit))
	}
	*tokenLimit -= amount

	var transferOptions []ledgerSystem.TransferOptions
	if asset == "hbd" {
		transferOptions = []ledgerSystem.TransferOptions{
			{
				Exclusion: ctx.rcLimit,
			},
		}
	}
	res := ctx.ledger.ExecuteTransfer(ledgerSystem.OpLogEvent{
		To:     ctx.env.ContractId,
		From:   ctx.env.RequiredAuths[0],
		Amount: amount,
		Asset:  asset,
		// Memo   string `json:"mo" // TODO add in future
		Type: "transfer",

		//Not parted of compiled state
		// Id          string `json:"id"`
		BlockHeight: ctx.env.BlockHeight,
	}, transferOptions...)
	if !res.Ok {
		return result.Err[struct{}](fmt.Errorf("%s", res.Msg))
	}
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) SendBalance(to string, amount int64, asset string) result.Result[struct{}] {
	res := ctx.ledger.ExecuteTransfer(ledgerSystem.OpLogEvent{
		From:   ctx.env.ContractId,
		To:     to,
		Amount: amount,
		Asset:  asset,
		// Memo   string `json:"mo" // TODO add in future
		Type: "transfer",

		//Not parted of compiled state
		// Id          string `json:"id"`
		BlockHeight: ctx.env.BlockHeight,
	})
	if !res.Ok {
		return result.Err[struct{}](fmt.Errorf("%s", res.Msg))
	}
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) WithdrawBalance(to string, amount int64, asset string) result.Result[struct{}] {
	res := ctx.ledger.Withdraw(ledgerSystem.WithdrawParams{
		From:   ctx.env.ContractId,
		To:     to,
		Amount: amount,
		Asset:  asset,
		// Memo   string `json:"mo" // TODO add in future

		//Not parted of compiled state
		// Id          string `json:"id"`
		BlockHeight: ctx.env.BlockHeight,
	})
	if !res.Ok {
		return result.Err[struct{}](fmt.Errorf("%s", res.Msg))
	}
	return result.Ok(struct{}{})
}

func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](err)
	}
	return result.Ok(res)
}
