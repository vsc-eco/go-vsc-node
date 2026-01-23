package contract_execution_context

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"unicode/utf8"
	"vsc-node/modules/common"
	"vsc-node/modules/common/params"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/contracts"
	tss_db "vsc-node/modules/db/vsc/tss"
	ledgerSystem "vsc-node/modules/ledger-system"

	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"
	wasm_types "vsc-node/modules/wasm/types"

	"github.com/JustinKnueppel/go-result"
	"go.mongodb.org/mongo-driver/mongo"
)

type contractExecutionContext struct {
	intents     []contracts.Intent
	ledger      ledgerSystem.LedgerSession
	env         Environment
	rcLimit     int64
	gasRemain   uint
	callSession *contract_session.CallSession
	recursion   int
	gasUsage    uint

	ioReadGas  int
	ioWriteGas int
	ioSessions []*ioSession

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

var _ wasm_context.ExecContextValue = &contractExecutionContext{}

func New(
	env Environment,
	rcLimit int64,
	gasRemain uint,
	ledger ledgerSystem.LedgerSession,
	callSession *contract_session.CallSession,
	recursionDepth int,
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
			seen := seenTypes[key]
			if seen {
				continue
			}
			seenTypes[key] = true
			val, _ := common.SafeParseHiveFloat(limit)
			tokenLimits[token] = &val
		}
	}
	return &contractExecutionContext{
		env.Intents,
		ledger,
		env,
		rcLimit,
		gasRemain,
		callSession,
		recursionDepth,
		0,
		0,
		0,
		nil,
		tokenLimits,
	}
}

func (ctx *contractExecutionContext) IOGas() int {
	return (ctx.ioReadGas*params.READ_IO_GAS_RC_COST + ctx.ioWriteGas*params.WRITE_IO_GAS_RC_COST) * params.CYCLE_GAS_PER_RC
}

type ioSession struct {
	ioReadGas  int
	ioWriteGas int
	end        func() int
}

func (ctx *contractExecutionContext) IOSession() wasm_context.IOSession {
	session := &ioSession{}
	session.end = func() int {
		i := slices.Index(ctx.ioSessions, session)
		ctx.ioSessions[i] = nil
		return (session.ioReadGas*params.READ_IO_GAS_RC_COST + session.ioWriteGas*params.WRITE_IO_GAS_RC_COST) * params.CYCLE_GAS_PER_RC
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
	ctx.callSession.Rollback()
}

func (ctx *contractExecutionContext) SetGasUsage(gasUsed uint) {
	ctx.gasUsage = gasUsed
}

func (ctx *contractExecutionContext) Log(msg string) {
	ctx.doIO(len(msg))
	ctx.callSession.AppendLogs(ctx.env.ContractId, msg)
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
	newWriteGas := ctx.callSession.IncSize(ctx.env.ContractId, newWriteToAdd)
	ctx.doIO(newWriteToAdd-newWriteGas, newWriteGas)
	ctx.callSession.GetStateStore(ctx.env.ContractId).Set(key, []byte(value))
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) GetState(key string) result.Result[string] {
	ctx.doIO(len(key))
	res := ctx.callSession.GetStateStore(ctx.env.ContractId).Get(key)
	if res == nil {
		return result.Ok("")
	}
	ctx.doIO(len(string(res)))
	return result.Ok(string(res))
}

func (ctx *contractExecutionContext) DeleteState(key string) result.Result[struct{}] {
	ctx.doIO(len(key))
	value := ctx.GetState(key).Unwrap()
	ctx.callSession.IncSize(ctx.env.ContractId, len(key)-len(value))
	ctx.callSession.GetStateStore(ctx.env.ContractId).Delete(key)
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

	// assuming sender is the RC payer
	var transferOptions []ledgerSystem.TransferOptions
	if asset == "hbd" && ctx.env.Caller == ctx.env.Sender {
		transferOptions = []ledgerSystem.TransferOptions{
			{
				Exclusion: ctx.rcLimit,
			},
		}
	}
	res := ctx.ledger.ExecuteTransfer(ledgerSystem.OpLogEvent{
		To:     "contract:" + ctx.env.ContractId,
		From:   ctx.env.Caller,
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

func (ctx *contractExecutionContext) ContractStateGet(contractId string, key string) result.Result[string] {
	ctx.doIO(len(key))
	res := ctx.callSession.GetStateStore(contractId).Get(key)
	if res == nil {
		return result.Ok("")
	}
	resStr := string(res)
	ctx.doIO(len(resStr))
	return result.Ok(resStr)
}

func (ctx *contractExecutionContext) ContractCall(contractId string, method string, payload string, options string) wasm_types.WasmResult {
	nextRecursion := ctx.recursion + 1
	if nextRecursion > params.CONTRACT_CALL_MAX_RECURSION_DEPTH {
		return result.Err[wasm_types.WasmResultStruct](errors.Join(fmt.Errorf(contracts.IC_RCSE_LIMIT_HIT), fmt.Errorf("call recursion limit hit")))
	}
	payloadJson := json.RawMessage(payload)
	if !utf8.Valid(payloadJson) {
		return result.Err[wasm_types.WasmResultStruct](errors.Join(fmt.Errorf(contracts.IC_INVALD_PAYLD), fmt.Errorf("payload does not parse to a UTF-8 string")))
	}
	opts := contracts.ICCallOptions{
		Intents: []contracts.Intent{},
	}
	json.Unmarshal([]byte(options), &opts)

	return result.Flatten(result.Map(
		ctx.callSession.GetContractFromDb(contractId, ctx.env.BlockHeight),
		func(ct contract_session.ContractWithCode) wasm_types.WasmResult {
			w := wasm_runtime_ipc.New()
			w.Init()
			gasRemaining := ctx.gasRemain - ctx.gasUsage

			ctxValue := New(Environment{
				ContractId:           contractId,
				ContractOwner:        ct.Info.Owner,
				BlockHeight:          ctx.env.BlockHeight,
				TxId:                 ctx.env.TxId,
				BlockId:              ctx.env.BlockId,
				Index:                ctx.env.Index,
				OpIndex:              ctx.env.OpIndex,
				Timestamp:            ctx.env.Timestamp,
				RequiredAuths:        ctx.env.RequiredAuths,
				RequiredPostingAuths: ctx.env.RequiredPostingAuths,
				Caller:               "contract:" + ctx.env.ContractId,
				Sender:               ctx.env.Sender,
				Intents:              opts.Intents,
			}, ctx.rcLimit, gasRemaining, ctx.ledger, ctx.callSession, nextRecursion)

			callPayload := payload
			json.Unmarshal([]byte(payloadJson), &callPayload)

			wasmCtx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(ct.Code))
			res := w.Execute(wasmCtx, gasRemaining, method, callPayload, ct.Info.Runtime)
			if res.Error != nil {
				return result.Err[wasm_types.WasmResultStruct](errors.Join(fmt.Errorf("%s", res.ErrorCode), fmt.Errorf("%s", *res.Error), fmt.Errorf("%d", res.Gas)))
			}
			return result.Ok(res)
		},
	))
}

func (ctx *contractExecutionContext) TssCreateKey(keyId string, keyType string) result.Result[string] {
	fullKey := ctx.env.ContractId + "-" + keyId
	_, err := ctx.callSession.TssKeys.FindKey(fullKey)

	if err == mongo.ErrNoDocuments {
		ctx.callSession.AppendTssLog(ctx.env.ContractId, tss_db.TssOp{
			Type:  "create",
			KeyId: fullKey,
			Args:  keyType,
		})
		return result.Ok("created")
	}
	if err == nil {
		return result.Ok("already_exists")
	}
	fmt.Println("err", err)
	return result.Err[string](errors.New("runtime error"))
}

func (ctx *contractExecutionContext) TssGetKey(keyId string) result.Result[string] {
	fullKey := ctx.env.ContractId + "-" + keyId

	tssKey, err := ctx.callSession.TssKeys.FindKey(fullKey)

	var status string
	if err == mongo.ErrNoDocuments {
		status = "null"
	}

	status = tssKey.Status

	res := status
	if status == "active" || status == "deleted" {
		res += "," + tssKey.PublicKey
		res += "," + string(tssKey.Algo)
	}

	return result.Ok(res)
}

func (ctx *contractExecutionContext) TssKeySign(keyId string, msg string) result.Result[string] {
	fullKey := ctx.env.ContractId + "-" + keyId

	keyInfo, err := ctx.callSession.TssKeys.FindKey(fullKey)

	if err == mongo.ErrNoDocuments {
		return result.Ok("fail")
	}

	if keyInfo.Status != "active" {
		return result.Ok("fail")
	}

	ctx.callSession.AppendTssLog(ctx.env.ContractId, tss_db.TssOp{
		Type:  "sign",
		KeyId: fullKey,
		Args:  msg,
	})
	return result.Ok("ok")
}

// wrap the result of json.Marshal as used by EnvVar(), joins with ENV_VAR_ERROR symbol if Err
func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), err))
	}
	return result.Ok(res)
}
