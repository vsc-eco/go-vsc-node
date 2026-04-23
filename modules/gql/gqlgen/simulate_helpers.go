package gqlgen

// Helpers for the SimulateContractCalls resolver. Kept in a separate file so
// gqlgen's regeneration does not strip them — gqlgen rewrites only files
// matching its configured resolver filename pattern, and relocates any
// non-resolver methods it finds in schema.resolvers.go to a trailing comment
// block (losing their imports in the process).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"strings"

	"vsc-node/modules/common/params"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/gql/model"
	ledgerSystem "vsc-node/modules/ledger-system"
	rc_system "vsc-node/modules/rc-system"
	stateEngine "vsc-node/modules/state-processing"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"

	"github.com/ipfs/go-cid"
)

// executeSimulatedCall runs a single contract call against the shared sessions.
// Returns (result, fatal): fatal=true means the caller should abort the
// simulation — sessions have already been reverted by this helper.
func (r *queryResolver) executeSimulatedCall(
	call SimulateContractCallInput,
	opIndex int,
	blockHeight uint64,
	blockId string,
	timestamp string,
	input SimulateContractCallsInput,
	ledgerSession ledgerSystem.LedgerSession,
	callSession *contract_session.CallSession,
) (SimulateContractCallResult, bool) {
	rcLimit := uint(call.RcLimit)
	if maxGas := ^uint(0) / params.CYCLE_GAS_PER_RC; rcLimit > maxGas {
		rcLimit = maxGas
	}

	info, err := r.Contracts.ContractById(call.ContractID, blockHeight)
	if err != nil {
		errMsg := err.Error()
		ledgerSession.Revert()
		callSession.Rollback()
		return SimulateContractCallResult{
			Success: false,
			ErrMsg:  &errMsg,
			RcUsed:  model.Int64(100),
			GasUsed: model.Uint64(0),
		}, true
	}

	c, err := cid.Decode(info.Code)
	if err != nil {
		errMsg := err.Error()
		ledgerSession.Revert()
		callSession.Rollback()
		return SimulateContractCallResult{
			Success: false,
			ErrMsg:  &errMsg,
			RcUsed:  model.Int64(100),
			GasUsed: model.Uint64(0),
		}, true
	}

	node, err := r.Da.Get(c, nil)
	if err != nil {
		errMsg := err.Error()
		ledgerSession.Revert()
		callSession.Rollback()
		return SimulateContractCallResult{
			Success: false,
			ErrMsg:  &errMsg,
			RcUsed:  model.Int64(100),
			GasUsed: model.Uint64(0),
		}, true
	}

	code := node.RawData()

	caller := ""
	if len(input.RequiredAuths) > 0 {
		caller = input.RequiredAuths[0]
	} else if len(input.RequiredPostingAuths) > 0 {
		caller = input.RequiredPostingAuths[0]
	}

	intents := make([]contracts.Intent, 0, len(call.Intents))
	for _, intent := range call.Intents {
		args := make(map[string]string)
		for k, v := range intent.Args {
			args[k] = fmt.Sprintf("%v", v)
		}
		intents = append(intents, contracts.Intent{
			Type: intent.Type,
			Args: args,
		})
	}

	ctxValue := contract_execution_context.New(
		contract_execution_context.Environment{
			ContractId:           call.ContractID,
			ContractOwner:        info.Owner,
			BlockHeight:          blockHeight,
			TxId:                 input.TxID,
			BlockId:              blockId,
			Index:                0,
			OpIndex:              opIndex,
			Timestamp:            timestamp,
			RequiredAuths:        input.RequiredAuths,
			RequiredPostingAuths: input.RequiredPostingAuths,
			Caller:               caller,
			Sender:               caller,
			Intents:              intents,
		},
		int64(rcLimit), rc_system.FreeRcRemaining(r.StateEngine.RcSystem.NewSession(ledgerSession), caller, blockHeight), rcLimit*params.CYCLE_GAS_PER_RC, ledgerSession, callSession, 0,
	)

	var payload string
	if err := json.Unmarshal([]byte(call.Payload), &payload); err != nil {
		payload = call.Payload
	}

	wasmCtx := context.WithValue(
		context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue),
		wasm_context.WasmExecCodeCtxKey,
		hex.EncodeToString(code),
	)

	w := wasm_runtime_ipc.New()
	w.Init()
	res := w.Execute(wasmCtx, rcLimit*params.CYCLE_GAS_PER_RC, call.Action, payload, info.Runtime)
	rcUsed := int64(math.Max(math.Ceil(float64(res.Gas)/params.CYCLE_GAS_PER_RC), 100))

	if res.Error != nil {
		ledgerSession.Revert()
		callSession.Rollback()
		errCode := string(res.ErrorCode)
		errMsg := *res.Error
		return SimulateContractCallResult{
			Success: false,
			Err:     &errCode,
			ErrMsg:  &errMsg,
			RcUsed:  model.Int64(rcUsed),
			GasUsed: model.Uint64(res.Gas),
		}, true
	}

	diff := callSession.GetStateDiff()
	logs := callSession.PopLogs()

	logsMap := make(model.Map)
	for k, v := range logs {
		logsMap[k] = v
	}

	diffMap := make(model.Map)
	for k, v := range diff {
		diffMap[k] = v
	}

	ret := res.Result
	return SimulateContractCallResult{
		Success:   true,
		Ret:       &ret,
		RcUsed:    model.Int64(rcUsed),
		GasUsed:   model.Uint64(res.Gas),
		Logs:      logsMap,
		StateDiff: diffMap,
	}, false
}

// normalizeDepositFrom matches the live state-processing path
// (state_engine.go:612 & siblings, which always do `"hive:" + from` before
// calling LedgerSystem.Deposit). Without this, a bare Hive username from the
// SDK would reach ResolveDepositTarget's default-fallback branch — which
// returns `from` verbatim — and the deposit would credit e.g. `tibfox`
// while the subsequent call's `required_auths` lookup targets `hive:tibfox`.
// The session cache misses, the swap's PullBalance reads 0, and the sim
// reports `insufficient balance` despite a successful deposit result.
// Accounts already prefixed (`hive:`, `did:`) pass through untouched.
func normalizeDepositFrom(from string) string {
	if from == "" {
		return from
	}
	if strings.HasPrefix(from, "hive:") || strings.HasPrefix(from, "did:") {
		return from
	}
	return "hive:" + from
}

// executeSimulatedDeposit credits the target account's in-session balance,
// mirroring production memo-parsing but bypassing LedgerDb. The credit is
// visible to any subsequent op via session.GetBalance, and is discarded by the
// deferred Revert() at the end of the simulation.
func (r *queryResolver) executeSimulatedDeposit(
	deposit SimulateDepositInput,
	opIndex int,
	blockHeight uint64,
	txID string,
	ledgerSession ledgerSystem.LedgerSession,
) (SimulateContractCallResult, bool) {
	memo := ""
	if deposit.Memo != nil {
		memo = *deposit.Memo
	}

	// Align with live state-engine behaviour: production only ever feeds
	// ResolveDepositTarget a `hive:`/`did:`-prefixed `from`. The sim helper
	// is the first place bare usernames can reach the resolver, so
	// normalize up front or the default-fallback returns a bare owner.
	from := normalizeDepositFrom(deposit.From)

	var owner string
	if deposit.To != nil && *deposit.To != "" {
		owner = ledgerSystem.ResolveDepositTarget("to="+*deposit.To, from)
	} else {
		owner = ledgerSystem.ResolveDepositTarget(memo, from)
	}

	ledgerSession.AppendLedger(ledgerSystem.LedgerUpdate{
		Id:          stateEngine.MakeTxId(txID, opIndex),
		BlockHeight: blockHeight,
		OpIdx:       int64(opIndex),
		Owner:       owner,
		Amount:      int64(deposit.Amount),
		Asset:       deposit.Asset,
		Memo:        memo,
		Type:        "deposit",
	})

	// Echo the resolved owner + amount back in Ret so SDKs can confirm
	// the memo/to parsed the way they expected. Small payload, big help
	// when debugging "I deposited X but the swap still fails" reports.
	ret := fmt.Sprintf(
		`{"owner":%q,"amount":%d,"asset":%q}`,
		owner, deposit.Amount, deposit.Asset,
	)
	return SimulateContractCallResult{
		Success: true,
		Ret:     &ret,
		RcUsed:  model.Int64(0),
		GasUsed: model.Uint64(0),
	}, false
}
