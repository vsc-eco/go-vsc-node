package gqlgen

// LOCALONLY-EMPTY-1 (testnet 11-16/17). The API reads contract state through a
// local-only view of the data layer, so that a caller cannot park the node on
// the network. A read through that view that FAILS (a block of the state is not
// in the local store at that moment, seen for up to about a second after a
// block changed the state) is answered as empty state. getStateByKeys passed
// that on as null for keys that exist, and simulateContractCalls ran the dry
// run on empty contract state: wrong balances, wrong pre-checks, bots sizing
// their calls from a dry run of nothing.
//
// Both resolvers now read through a view of their own that records such a
// failure (datalayer.LocalOnlyTracking). When it happened the request reads
// again, after stateReadWaits; if the state still cannot be read the answer is
// the error StateNotReadable, never empty state.
//
// The bodies of the two resolvers are here unchanged, reading from the view
// they are given (they were in schema.resolvers.go and read the shared
// local-only view).

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"time"
	"vsc-node/lib/datalayer"
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

// StateNotReadable starts the error of a state read or a dry run the node could
// not serve because the contract's state was not readable from its own store at
// that moment. Clients match on it (the EVM bot holds a vault op for it), so it
// does not change.
const StateNotReadable = "state not readable right now"

// stateReadWaits are the pauses before each further read of a request whose
// state read failed: 1.5 s in all, over the longest window seen on testnet
// (about 0.9 s).
var stateReadWaits = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}

// readableState runs read on a tracking local-only view of da. If a state read
// through the view failed, the result is discarded and read runs again on a
// fresh view after the next wait; after the last wait the answer is the
// StateNotReadable error.
func readableState[T any](ctx context.Context, da *datalayer.DataLayer, read func(local *datalayer.DataLayer) (T, error)) (T, error) {
	for attempt := 0; ; attempt++ {
		local := da.LocalOnlyTracking()
		out, err := read(local)
		if !local.Missed() {
			return out, err
		}
		var zero T
		if attempt >= len(stateReadWaits) {
			return zero, errors.New(StateNotReadable + ": this node could not read the contract's state from its own store; try again")
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(stateReadWaits[attempt]):
		}
	}
}

// getStateByKeysOn is getStateByKeys reading from local.
func (r *queryResolver) getStateByKeysOn(ctx context.Context, contractID string, keys []string, encoding *string, local *datalayer.DataLayer) (model.Map, error) {
	if len(keys) < 1 || len(keys) > 100 {
		return nil, fmt.Errorf("number of state keys to query must be between 1 and 100")
	}
	output, err := r.ContractsState.GetLastOutput(contractID, math.MaxInt64)
	if err != nil {
		return nil, err
	}
	cidz, err := cid.Parse(output.StateMerkle)
	if err != nil {
		return nil, err
	}
	databin := datalayer.NewDataBinFromCid(local, cidz)
	result := make(map[string]interface{})
	var keyErr error
	for _, key := range keys {
		cidVal, err := databin.Get(key)
		if err != nil {
			if err == os.ErrNotExist {
				result[key] = nil
			} else {
				keyErr = err
			}
			continue
		}
		rawVal, err := local.GetRaw(*cidVal)
		if err != nil {
			keyErr = err
			continue
		}
		if encoding != nil && *encoding == "hex" {
			result[key] = hex.EncodeToString(rawVal)
		} else {
			result[key] = string(rawVal)
		}
	}
	return model.Map(result), keyErr
}

// simulateContractCallsOn is simulateContractCalls reading from local.
func (r *queryResolver) simulateContractCallsOn(ctx context.Context, input SimulateContractCallsInput, local *datalayer.DataLayer) ([]SimulateContractCallResult, error) {
	if !simulateRateLimiter.Allow() {
		return nil, fmt.Errorf("rate limit exceeded: max %d simulation requests per minute", simulateRateLimit)
	}

	if len(input.Calls) > 10 {
		return nil, fmt.Errorf("maximum 10 calls per simulation")
	}

	// Pentest finding F14: the resolver previously accepted any
	// value (including the empty list and unprefixed garbage) for
	// required_auths and required_posting_auths and derived caller
	// from the first entry without sanity-checking either list.
	// That made simulation a probing primitive: an attacker could
	// simulate as any user, real or fabricated. Validate format
	// here so simulation at least matches the on-chain auth shape.
	if len(input.RequiredAuths) == 0 && len(input.RequiredPostingAuths) == 0 {
		return nil, fmt.Errorf("simulateContractCalls: at least one required_auths or required_posting_auths entry is required")
	}
	for _, a := range input.RequiredAuths {
		if !strings.HasPrefix(a, "hive:") && !strings.HasPrefix(a, "did:") {
			return nil, fmt.Errorf("simulateContractCalls: required_auths entry %q must start with hive: or did:", a)
		}
	}
	for _, a := range input.RequiredPostingAuths {
		if !strings.HasPrefix(a, "hive:") && !strings.HasPrefix(a, "did:") {
			return nil, fmt.Errorf("simulateContractCalls: required_posting_auths entry %q must start with hive: or did:", a)
		}
	}

	// Get latest block info
	highestBlock, err := r.HiveBlocks.GetHighestBlock()
	if err != nil {
		return nil, fmt.Errorf("failed to get highest block: %w", err)
	}
	block, err := r.HiveBlocks.GetBlock(highestBlock)
	if err != nil {
		return nil, fmt.Errorf("failed to get block %d: %w", highestBlock, err)
	}

	blockHeight := block.BlockNumber
	blockId := block.BlockID
	timestamp := block.Timestamp

	// Validate rc_limit for all calls
	for i, call := range input.Calls {
		if call.RcLimit < 1 || call.RcLimit > 100000 {
			return nil, fmt.Errorf("call %d: rc_limit must be between 1 and 100000, got %d", i, call.RcLimit)
		}
	}

	// Create sessions from live DB state
	ledgerSession := ledgerSystem.NewSession(r.StateEngine.LedgerState)
	callSession := contract_session.NewCallSession(local, r.Contracts, r.ContractsState, r.TssKeys, r.TssCommitments, blockHeight, nil)

	results := make([]SimulateContractCallResult, 0, len(input.Calls))

	for opIndex, call := range input.Calls {
		rcLimit := uint64(call.RcLimit)
		// Cap rcLimit to prevent overflow when multiplied by CYCLE_GAS_PER_RC.
		// uint64 (not platform-dependent uint) keeps the bound identical
		// across architectures, matching TxVscCallContract.ExecuteTx.
		if maxGas := ^uint64(0) / params.CYCLE_GAS_PER_RC; rcLimit > maxGas {
			rcLimit = maxGas
		}
		// uint64->uint narrowing forced by WasmEdge's uint gas meter; lossless
		// on 64-bit and bounded by the maxGas cap above. See
		// TxVscCallContract.ExecuteTx for the full rationale.
		gasCycles := uint(rcLimit * params.CYCLE_GAS_PER_RC)

		info, err := r.Contracts.ContractById(call.ContractID, blockHeight)
		if err != nil {
			errMsg := err.Error()
			results = append(results, SimulateContractCallResult{
				Success: false,
				ErrMsg:  &errMsg,
				RcUsed:  model.Int64(100),
				GasUsed: model.Uint64(0),
			})
			ledgerSession.Revert()
			callSession.Rollback()
			return results, nil
		}

		c, err := cid.Decode(info.Code)
		if err != nil {
			errMsg := err.Error()
			results = append(results, SimulateContractCallResult{
				Success: false,
				ErrMsg:  &errMsg,
				RcUsed:  model.Int64(100),
				GasUsed: model.Uint64(0),
			})
			ledgerSession.Revert()
			callSession.Rollback()
			return results, nil
		}

		node, err := local.Get(c, nil)
		if err != nil {
			errMsg := err.Error()
			results = append(results, SimulateContractCallResult{
				Success: false,
				ErrMsg:  &errMsg,
				RcUsed:  model.Int64(100),
				GasUsed: model.Uint64(0),
			})
			ledgerSession.Revert()
			callSession.Rollback()
			return results, nil
		}

		code := node.RawData()

		caller := ""
		if len(input.RequiredAuths) > 0 {
			caller = input.RequiredAuths[0]
		} else if len(input.RequiredPostingAuths) > 0 {
			caller = input.RequiredPostingAuths[0]
		}

		// Convert intents
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

		var pendulumOracle map[string]interface{}
		var ctxOpts []contract_execution_context.Option
		if r.StateEngine != nil {
			pendulumOracle = r.StateEngine.PendulumOracleEnv()
			// Wire the pendulum swap-fee applier so simulated swaps on
			// whitelisted pools exercise the same fee/accrual path as
			// on-chain execution instead of failing with "pendulum applier
			// not configured". The applier reads geometry from the live
			// FeedTracker and accrues against this simulation's ledger
			// session, which is reverted at the end — nothing is committed.
			if applier := r.StateEngine.PendulumApplier(); applier != nil {
				ctxOpts = append(ctxOpts, contract_execution_context.WithPendulumApplier(applier))
			}
			// The same version- and config-gated rules the chain runs this call
			// with (stateEngine.ContractCallGates: try/catch, the TssGetKey fields,
			// SP1 pricing, big swap amounts, the TSS key allowlist), from the one
			// list the real call uses. A preview that lacks one of them answers
			// something the chain would not: without try/catch a deposit whose
			// swap fails aborted in the dry run, where the real call catches the
			// failure and credits the deposit (SIM-TRY-1). Read-only (reverted
			// like the rest), never the state root.
			activeVersion := r.StateEngine.ActiveConsensusVersion(blockHeight)
			ctxOpts = append(ctxOpts, stateEngine.ContractCallGates(r.StateEngine.SystemConfig(), blockHeight, activeVersion)...)
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
				PendulumOracle:       pendulumOracle,
			},
			int64(rcLimit), rc_system.FreeRcRemaining(r.StateEngine.RcSystem.NewSession(ledgerSession), caller, blockHeight), gasCycles, ledgerSession, callSession, 0,
			ctxOpts...,
		)

		// Unmarshal payload string
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
		res := w.Execute(wasmCtx, gasCycles, call.Action, payload, info.Runtime)
		rcUsed := int64(math.Max(math.Ceil(float64(res.Gas)/params.CYCLE_GAS_PER_RC), 100))

		if res.Error != nil {
			ledgerSession.Revert()
			callSession.Rollback()
			errCode := string(res.ErrorCode)
			errMsg := *res.Error
			results = append(results, SimulateContractCallResult{
				Success: false,
				Err:     &errCode,
				ErrMsg:  &errMsg,
				RcUsed:  model.Int64(rcUsed),
				GasUsed: model.Uint64(res.Gas),
			})
			return results, nil
		}

		diff := callSession.GetStateDiff()
		logs := callSession.PopLogs()

		// Convert logs to map[string]interface{}
		logsMap := make(model.Map)
		for k, v := range logs {
			logsMap[k] = v
		}

		// Convert state diff to map[string]interface{}
		diffMap := make(model.Map)
		for k, v := range diff {
			diffMap[k] = v
		}

		ret := res.Result
		results = append(results, SimulateContractCallResult{
			Success:   true,
			Ret:       &ret,
			RcUsed:    model.Int64(rcUsed),
			GasUsed:   model.Uint64(res.Gas),
			Logs:      logsMap,
			StateDiff: diffMap,
		})
	}

	// After all calls succeed, collect results but do not mutate live state.
	// Done() would append to the live LedgerState oplog/virtual ledger,
	// corrupting production balances from a read-only simulation endpoint.
	ledgerSession.Revert()

	return results, nil
}
