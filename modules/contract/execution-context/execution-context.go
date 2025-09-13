package contract_execution_context

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"vsc-node/modules/common"
	"vsc-node/modules/db/vsc/contracts"
	ledgerSystem "vsc-node/modules/ledger-system"

	"github.com/JustinKnueppel/go-result"
)

type contractExecutionContext struct {
	intents     []contracts.Intent
	ledger      ledgerSystem.LedgerSession
	env         Environment
	rcLimit     int64
	storage     StateStore
	currentSize int
	maxSize     int

	logs []string

	ioReadGas  int
	ioWriteGas int
	ioSessions []IOSession

	tokenLimits map[string]*int64
}

type ContractExecutionContext = *contractExecutionContext

type StateStore interface {
	Get(key string) []byte
	Set(key string, value []byte)
	Delete(key string)
	Commit()
	Rollback()
}

type Environment struct {
	ContractId           string
	ContractOwner        string
	BlockHeight          uint64
	TxId                 string
	BlockId              string
	Index                int
	OpIndex              int
	Timestamp            string
	RequiredAuths        []string
	RequiredPostingAuths []string
	Caller               string
	Sender               string
	Intents              []contracts.Intent
}

// var _ wasm_context.ExecContextValue = &contractExecutionContext{}

func New(
	env Environment,
	rcLimit int64,
	ledger ledgerSystem.LedgerSession,
	storage StateStore,
	metadata contracts.ContractMetadata,
) ContractExecutionContext {
	seenTypes := make(map[string]bool)
	tokenLimits := make(map[string]*int64)
	for _, intent := range env.Intents {
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
	// ensureInternalStorageInitialized(interalStorage)
	return &contractExecutionContext{
		env.Intents,
		ledger,
		env,
		rcLimit,
		storage,
		metadata.CurrentSize,
		metadata.MaxSize,
		nil,
		0,
		0,
		nil,
		tokenLimits,
	}
}

// func ensureInternalStorageInitialized(internalStorage map[string]interface{}) {
// 	_, ok := internalStorage["current_size"]
// 	if !ok {
// 		internalStorage["current_size"] = 0
// 	}
// 	_, ok = internalStorage["max_size"]
// 	if !ok {
// 		internalStorage["max_size"] = 0
// 	}
// }

func (ctx *contractExecutionContext) Logs() []string {
	return ctx.logs
}

func (ctx *contractExecutionContext) InternalStorage() contracts.ContractMetadata {
	return contracts.ContractMetadata{
		CurrentSize: ctx.currentSize,
		MaxSize:     ctx.maxSize,
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
	case "contract.id":
		return result.Ok(ctx.env.ContractId)
	case "contract.owner":
		return result.Ok(ctx.env.ContractOwner)
	case "tx.id": // tx ID
		return result.Ok(ctx.env.TxId)
	case "tx.index":
		return result.Ok(fmt.Sprint(ctx.env.Index))
	case "tx.op_index":
		return result.Ok(fmt.Sprint(ctx.env.OpIndex))
	case "tx.origin":
		fallthrough
	case "msg.sender":
		if len(ctx.env.RequiredAuths) > 0 {
			return result.Ok(ctx.env.RequiredAuths[0])
		}
		if len(ctx.env.RequiredPostingAuths) > 0 {
			return result.Ok(ctx.env.RequiredPostingAuths[0])
		}
		return result.Err[string](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), fmt.Errorf("no value for %s", key)))
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
	case "msg.payer":
		//TODO: implement payer logic
		if len(ctx.env.RequiredAuths) > 0 {
			return result.Ok(ctx.env.RequiredAuths[0])
		} else if len(ctx.env.RequiredPostingAuths) > 0 {
			return result.Ok(ctx.env.RequiredPostingAuths[0])
		}
	case "msg.caller":
		return result.Ok(ctx.env.Caller)
	case "intents":
		return result.Map(
			resultWrap(json.Marshal(ctx.env.Intents)),
			func(b []byte) string {
				return string(b)
			},
		)
	case "block.id": // block ID
		return result.Ok(ctx.env.BlockId)
	case "block.height":
		return result.Ok(fmt.Sprint(ctx.env.BlockHeight))
	case "block.timestamp":
		return result.Ok(ctx.env.Timestamp)
	}
	return result.Err[string](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), fmt.Errorf("environment does not contain value for \"%s\"", key)))
}

// GetEnv returns the environment variables in a standard contract format
func (ctx *contractExecutionContext) GetEnv() result.Result[string] {
	var payer string
	if len(ctx.env.RequiredAuths) > 0 {
		payer = ctx.env.RequiredAuths[0]
	} else if len(ctx.env.RequiredPostingAuths) > 0 {
		payer = ctx.env.RequiredPostingAuths[0]
	}

	if ctx.env.RequiredAuths == nil {
		ctx.env.RequiredAuths = []string{}
	}

	if ctx.env.RequiredPostingAuths == nil {
		ctx.env.RequiredPostingAuths = []string{}
	}

	envMap := map[string]interface{}{
		// contract section
		"contract.id":    ctx.env.ContractId,
		"contract.owner": ctx.env.ContractOwner,

		// tx section
		"tx.id":       ctx.env.TxId,
		"tx.index":    ctx.env.Index,
		"tx.op_index": ctx.env.OpIndex,

		// block section
		"block.id":        ctx.env.BlockId,
		"block.height":    ctx.env.BlockHeight,
		"block.timestamp": ctx.env.Timestamp,

		//msg section
		"msg.sender":                 ctx.env.Sender,
		"msg.required_auths":         ctx.env.RequiredAuths,
		"msg.required_posting_auths": ctx.env.RequiredPostingAuths,
		"msg.payer":                  payer,
		"msg.caller":                 ctx.env.Caller,
		"intents":                    ctx.env.Intents,
	}

	envBytes, err := json.Marshal(envMap)
	if err != nil {
		return result.Err[string](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), fmt.Errorf("failed to marshal env map: %w", err)))
	}
	return result.Ok(string(envBytes))
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
		return result.Ok("")
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
	getBal := ctx.ledger.GetBalance(account, ctx.env.BlockHeight, asset)

	return getBal
}

func (ctx *contractExecutionContext) PullBalance(amount int64, asset string) result.Result[struct{}] {
	if len(ctx.env.RequiredAuths) == 0 {
		return result.Err[struct{}](errors.Join(fmt.Errorf(contracts.MISSING_REQ_AUTH), fmt.Errorf("no active authority")))
	}
	tokenLimit, ok := ctx.tokenLimits[asset]
	if !ok {
		return result.Err[struct{}](errors.Join(fmt.Errorf(contracts.LEDGER_INTENT_ERROR), fmt.Errorf("no user intent for: %s", asset)))
	}
	if amount > *tokenLimit {
		return result.Err[struct{}](errors.Join(fmt.Errorf(contracts.LEDGER_INTENT_ERROR), fmt.Errorf("amount (%d) is over remaining token limit (%d)", amount, *tokenLimit)))
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
		To:     "contract:" + ctx.env.ContractId,
		From:   ctx.env.RequiredAuths[0],
		Amount: amount,
		Asset:  asset,
		// Memo   string `json:"mo" // TODO add in future
		Type: "transfer",

		//Not parted of compiled state
		// Id          string `json:"id"`
		Id:          ctx.env.TxId,
		BlockHeight: ctx.env.BlockHeight,
	}, transferOptions...)
	if !res.Ok {
		return result.Err[struct{}](errors.Join(fmt.Errorf(contracts.LEDGER_ERROR), fmt.Errorf("%s", res.Msg)))
	}
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) SendBalance(to string, amount int64, asset string) result.Result[struct{}] {
	res := ctx.ledger.ExecuteTransfer(ledgerSystem.OpLogEvent{
		From:   "contract:" + ctx.env.ContractId,
		To:     to,
		Amount: amount,
		Asset:  asset,
		// Memo   string `json:"mo" // TODO add in future
		Type: "transfer",

		//Not parted of compiled state
		// Id          string `json:"id"`
		Id:          ctx.env.TxId,
		BlockHeight: ctx.env.BlockHeight,
	})
	if !res.Ok {
		return result.Err[struct{}](errors.Join(fmt.Errorf(contracts.LEDGER_ERROR), fmt.Errorf("%s", res.Msg)))
	}
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) WithdrawBalance(to string, amount int64, asset string) result.Result[struct{}] {
	res := ctx.ledger.Withdraw(ledgerSystem.WithdrawParams{
		From:   "contract:" + ctx.env.ContractId,
		To:     to,
		Amount: amount,
		Asset:  asset,
		// Memo   string `json:"mo" // TODO add in future

		//Not parted of compiled state
		// Id          string `json:"id"`
		Id:          ctx.env.TxId,
		BlockHeight: ctx.env.BlockHeight,
	})
	if !res.Ok {
		return result.Err[struct{}](errors.Join(fmt.Errorf(contracts.LEDGER_ERROR), fmt.Errorf("%s", res.Msg)))
	}
	return result.Ok(struct{}{})
}

// wrap the result of json.Marshal as used by EnvVar(), joins with ENV_VAR_ERROR symbol if Err
func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), err))
	}
	return result.Ok(res)
}
