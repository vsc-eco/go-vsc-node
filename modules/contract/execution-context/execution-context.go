package contract_execution_context

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
	"vsc-node/lib/vsclog"
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

var tssLog = vsclog.Module("tss")

type contractExecutionContext struct {
	ledger      ledgerSystem.LedgerSession
	env         Environment
	rcLimit     int64
	// Remaining RCs the RC payer can consume from their free tier (e.g.
	// RC_HIVE_FREE_AMOUNT for Hive accounts minus already-frozen RCs). Used
	// by PullBalance to decide how much HBD to reserve against RC consumption.
	// Zero when the source isn't the RC payer (inter-contract calls) or when
	// the payer has no free-tier capacity left.
	rcFreeRemaining int64
	gasRemain       uint
	callSession     *contract_session.CallSession
	recursion       int
	gasUsage        uint

	ioReadGas  int
	ioWriteGas int
	ioSessions []*ioSession

	tokenLimits map[string]*int64

	pendulumApplier wasm_context.PendulumApplier
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
	// PendulumOracle is merged into wasm env JSON (system.get_env) and visible via system.get_env_key.
	PendulumOracle map[string]interface{}
}

var _ wasm_context.ExecContextValue = &contractExecutionContext{}

func New(
	env Environment,
	rcLimit int64,
	rcFreeRemaining int64,
	gasRemain uint,
	ledger ledgerSystem.LedgerSession,
	callSession *contract_session.CallSession,
	recursionDepth int,
	opts ...Option,
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
			decimals := -1
			decimalsStr, ok := intent.Args["decimals"]
			if ok {
				dec, err := strconv.Atoi(decimalsStr)
				if err == nil {
					decimals = dec
				}
			}
			key := intent.Type + "-" + token
			seen := seenTypes[key]
			if seen {
				continue
			}
			seenTypes[key] = true
			val, _ := common.ParseDecimalsToBaseUnits(limit, decimals)
			tokenLimits[token] = &val
		}
	}
	ctx := &contractExecutionContext{
		ledger:          ledger,
		env:             env,
		rcLimit:         rcLimit,
		rcFreeRemaining: rcFreeRemaining,
		gasRemain:       gasRemain,
		callSession:     callSession,
		recursion:       recursionDepth,
		tokenLimits:     tokenLimits,
	}
	for _, opt := range opts {
		opt(ctx)
	}
	return ctx
}

// Option mutates a fresh contractExecutionContext during construction.
// Options exist so callers can wire in deps (currently the pendulum
// applier) without growing New's positional arg list and breaking the
// existing call sites that don't need them.
type Option func(*contractExecutionContext)

func WithPendulumApplier(p wasm_context.PendulumApplier) Option {
	return func(ctx *contractExecutionContext) { ctx.pendulumApplier = p }
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
	if ctx.env.PendulumOracle != nil {
		if v, ok := ctx.env.PendulumOracle[key]; ok {
			return envOracleValueToResult(v)
		}
	}
	return result.Err[string](
		errors.Join(
			fmt.Errorf(contracts.ENV_VAR_ERROR),
			fmt.Errorf("environment does not contain value for \"%s\"", key),
		),
	)
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
	for k, v := range ctx.env.PendulumOracle {
		envMap[k] = v
	}

	envBytes, err := json.Marshal(envMap)
	if err != nil {
		return result.Err[string](
			errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), fmt.Errorf("failed to marshal env map: %w", err)),
		)
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

func (ctx *contractExecutionContext) GetEphemState(contractId string, key string) result.Result[string] {
	c := contractId
	if c == "" {
		c = ctx.env.ContractId
	}
	res := ctx.callSession.GetStateStore(c).GetEphem(key)
	return result.Ok(string(res))
}

func (ctx *contractExecutionContext) SetEphemState(key string, value string) result.Result[struct{}] {
	ctx.callSession.GetStateStore(ctx.env.ContractId).SetEphem(key, []byte(value))
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) DeleteEphemState(key string) result.Result[struct{}] {
	ctx.callSession.GetStateStore(ctx.env.ContractId).DeleteEphem(key)
	return result.Ok(struct{}{})
}

func (ctx *contractExecutionContext) GetBalance(account string, asset string) int64 {
	getBal := ctx.ledger.GetBalance(account, ctx.env.BlockHeight, asset)

	return getBal
}

func (ctx *contractExecutionContext) PullBalance(from string, amount int64, asset string) result.Result[struct{}] {
	if len(ctx.env.RequiredAuths) == 0 {
		return result.Err[struct{}](
			errors.Join(fmt.Errorf(contracts.MISSING_REQ_AUTH), fmt.Errorf("no active authority")),
		)
	}
	switch from {
	case ctx.env.Caller, "":
		tokenLimit, ok := ctx.tokenLimits[asset]
		if !ok {
			return result.Err[struct{}](
				errors.Join(fmt.Errorf(contracts.LEDGER_INTENT_ERROR), fmt.Errorf("no caller intent for: %s", asset)),
			)
		}
		if amount > *tokenLimit {
			return result.Err[struct{}](
				errors.Join(
					fmt.Errorf(contracts.LEDGER_INTENT_ERROR),
					fmt.Errorf("amount (%d) is over remaining token limit (%d)", amount, *tokenLimit),
				),
			)
		}
		*tokenLimit -= amount
		from = ctx.env.Caller
	default:
		return result.Err[struct{}](
			errors.Join(errors.New(contracts.LEDGER_INTENT_ERROR), errors.New("user does not match caller")),
		)
	}

	// Reserve HBD for RC consumption only when the source is the RC payer.
	// Inter-contract pulls (from = "contract:...") originate from a contract
	// that isn't paying RCs, so no exclusion applies. The RC payer's free
	// tier (RC_HIVE_FREE_AMOUNT minus what's already frozen) covers part of
	// rcLimit without needing HBD backing — only the remainder is reserved.
	var transferOptions []ledgerSystem.TransferOptions
	if asset == "hbd" && !strings.HasPrefix(from, "contract:") {
		exclusion := ctx.rcLimit - ctx.rcFreeRemaining
		if exclusion > 0 {
			transferOptions = []ledgerSystem.TransferOptions{
				{Exclusion: exclusion},
			}
		}
	}
	res := ctx.ledger.ExecuteTransfer(ledgerSystem.OpLogEvent{
		To:     "contract:" + ctx.env.ContractId,
		From:   from,
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

func (ctx *contractExecutionContext) PendulumApplySwapFees(args wasm_context.PendulumSwapFeeArgs) result.Result[wasm_context.PendulumSwapFeeResult] {
	if ctx.pendulumApplier == nil {
		return result.Err[wasm_context.PendulumSwapFeeResult](
			errors.Join(fmt.Errorf(contracts.SDK_ERROR), fmt.Errorf("pendulum applier not configured")),
		)
	}
	// Construct the per-call accrual closure bound to the active ledger
	// session. The applier invokes it with the HBD amount to move from the
	// pool contract's account into the global pendulum:nodes bucket.
	// ExecuteTransfer is the same primitive hive.transfer / hive.draw use,
	// so the accrual is a paired (debit, credit) ledger op — no minting.
	//
	// Id matches the convention used by SendBalance/PullBalance: pass the
	// bare TxId. ledgerSession.AppendOplog disambiguates multiple
	// ExecuteTransfer calls in one session via its `idCache` (txId → txId:1
	// → txId:2 …); manually appending a tag here would bypass that
	// machinery and break callers that correlate ledger records by the
	// canonical {txId}[:N]#in/out shape.
	accrue := func(amountHBD int64) error {
		if amountHBD <= 0 {
			return nil
		}
		res := ctx.ledger.ExecuteTransfer(ledgerSystem.OpLogEvent{
			From:        "contract:" + ctx.env.ContractId,
			To:          ledgerSystem.PendulumNodesHBDBucket,
			Amount:      amountHBD,
			Asset:       "hbd",
			Type:        "transfer",
			Id:          ctx.env.TxId,
			BlockHeight: ctx.env.BlockHeight,
		})
		if !res.Ok {
			return errors.New(res.Msg)
		}
		return nil
	}
	return ctx.pendulumApplier.ApplySwapFees(ctx.env.ContractId, ctx.env.TxId, ctx.env.BlockHeight, args, accrue)
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

func (ctx *contractExecutionContext) ContractCall(
	contractId string,
	method string,
	payload string,
	options string,
) wasm_types.WasmResult {
	nextRecursion := ctx.recursion + 1
	if nextRecursion > params.CONTRACT_CALL_MAX_RECURSION_DEPTH {
		return result.Err[wasm_types.WasmResultStruct](
			errors.Join(fmt.Errorf(contracts.IC_RCSE_LIMIT_HIT), fmt.Errorf("call recursion limit hit")),
		)
	}
	payloadJson := json.RawMessage(payload)
	if !utf8.Valid(payloadJson) {
		return result.Err[wasm_types.WasmResultStruct](
			errors.Join(fmt.Errorf(contracts.IC_INVALD_PAYLD), fmt.Errorf("payload does not parse to a UTF-8 string")),
		)
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
			}, ctx.rcLimit, 0, gasRemaining, ctx.ledger, ctx.callSession, nextRecursion)

			callPayload := payload
			json.Unmarshal([]byte(payloadJson), &callPayload)

			wasmCtx := context.WithValue(
				context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue),
				wasm_context.WasmExecCodeCtxKey,
				hex.EncodeToString(ct.Code),
			)
			res := w.Execute(wasmCtx, gasRemaining, method, callPayload, ct.Info.Runtime)
			if res.Error != nil {
				return result.Err[wasm_types.WasmResultStruct](
					errors.Join(
						fmt.Errorf("%s", res.ErrorCode),
						fmt.Errorf("%s", *res.Error),
						fmt.Errorf("%d", res.Gas),
					),
				)
			}
			return result.Ok(res)
		},
	))
}

func (ctx *contractExecutionContext) TssCreateKey(keyId string, keyType string, epochs uint64) result.Result[string] {
	fullKey := ctx.env.ContractId + "-" + keyId
	_, err := ctx.callSession.TssKeys.FindKey(fullKey)

	if err == mongo.ErrNoDocuments {
		ctx.callSession.AppendTssLog(ctx.env.ContractId, tss_db.TssOp{
			Type:   "create",
			KeyId:  fullKey,
			Args:   keyType,
			Epochs: epochs,
		})
		return result.Ok("created")
	}
	if err == nil {
		return result.Ok("already_exists")
	}
	fmt.Println("err", err)
	return result.Err[string](errors.New("runtime error"))
}

func (ctx *contractExecutionContext) TssRenewKey(keyId string, additionalEpochs uint64) result.Result[string] {
	fullKey := ctx.env.ContractId + "-" + keyId
	key, err := ctx.callSession.TssKeys.FindKey(fullKey)

	if err == mongo.ErrNoDocuments {
		return result.Err[string](errors.New("key not found"))
	}
	if err != nil {
		return result.Err[string](errors.New("runtime error"))
	}
	if key.Status == tss_db.TssKeyRetired {
		return result.Err[string](errors.New("key is retired and cannot be renewed"))
	}
	// Active keys with no expiry were intentionally created without a lifespan and cannot be renewed.
	// Deprecated keys with ExpiryEpoch==0 are legacy keys — renewal is allowed and the state engine
	// will base the new expiry on the current epoch.
	if key.Status == tss_db.TssKeyActive && key.ExpiryEpoch == 0 {
		return result.Err[string](errors.New("key has no expiry to renew"))
	}

	ctx.callSession.AppendTssLog(ctx.env.ContractId, tss_db.TssOp{
		Type:   "renew",
		KeyId:  fullKey,
		Epochs: additionalEpochs,
	})
	return result.Ok("renewed")
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
	if status == tss_db.TssKeyActive || status == tss_db.TssKeyDeprecated || status == "deleted" {
		res += "," + tssKey.PublicKey
		res += "," + string(tssKey.Algo)
	}

	return result.Ok(res)
}

func (ctx *contractExecutionContext) TssKeySign(keyId string, msg string) result.Result[string] {
	fullKey := ctx.env.ContractId + "-" + keyId

	keyInfo, err := ctx.callSession.TssKeys.FindKey(fullKey)

	if err == mongo.ErrNoDocuments {
		tssLog.Verbose("key sign rejected: key not found", "keyId", fullKey)
		return result.Ok("fail")
	}

	if keyInfo.Status != tss_db.TssKeyActive {
		tssLog.Verbose("key sign rejected: not active", "keyId", fullKey, "status", keyInfo.Status)
		return result.Ok("fail")
	}

	ctx.callSession.AppendTssLog(ctx.env.ContractId, tss_db.TssOp{
		Type:  "sign",
		KeyId: fullKey,
		Args:  msg,
	})
	return result.Ok("ok")
}

// envOracleValueToResult stringifies values from PendulumOracle for system.get_env_key.
func envOracleValueToResult(v interface{}) result.Result[string] {
	switch t := v.(type) {
	case string:
		return result.Ok(t)
	case bool:
		return result.Ok(strconv.FormatBool(t))
	case float64:
		return result.Ok(strconv.FormatFloat(t, 'g', -1, 64))
	case float32:
		return result.Ok(strconv.FormatFloat(float64(t), 'g', -1, 32))
	case int:
		return result.Ok(strconv.Itoa(t))
	case int32:
		return result.Ok(strconv.FormatInt(int64(t), 10))
	case int64:
		return result.Ok(strconv.FormatInt(t, 10))
	case uint:
		return result.Ok(strconv.FormatUint(uint64(t), 10))
	case uint32:
		return result.Ok(strconv.FormatUint(uint64(t), 10))
	case uint64:
		return result.Ok(strconv.FormatUint(t, 10))
	case []string:
		b, err := json.Marshal(t)
		if err != nil {
			return result.Err[string](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), err))
		}
		return result.Ok(string(b))
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return result.Err[string](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), err))
		}
		return result.Ok(string(b))
	}
}

// wrap the result of json.Marshal as used by EnvVar(), joins with ENV_VAR_ERROR symbol if Err
func resultWrap[T any](res T, err error) result.Result[T] {
	if err != nil {
		return result.Err[T](errors.Join(fmt.Errorf(contracts.ENV_VAR_ERROR), err))
	}
	return result.Ok(res)
}
