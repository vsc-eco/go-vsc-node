package test_utils

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math"
	"vsc-node/lib/logger"
	"vsc-node/modules/common"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
	stateEngine "vsc-node/modules/state-processing"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime "vsc-node/modules/wasm/runtime"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"
)

func randomHex(n int) string {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return ""
	}
	return hex.EncodeToString(bytes)
}

// Contract testing environment
type ContractTest struct {
	BlockHeight   uint64
	Contracts     map[string][]byte
	LedgerSession *stateEngine.LedgerSession
	State         map[string]contract_execution_context.StateStore
	StateEngine   *stateEngine.StateEngine
}

// Create a new contract testing environment with mock databases.
func NewContractTest() ContractTest {
	logr := logger.PrefixedLogger{Prefix: "contract-test"}
	ledgers := MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	balances := MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	interestClaims := MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	actions := MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	elections := MockElectionDb{}
	se := stateEngine.New(logr, nil, nil, &elections, nil, nil, nil, &ledgers, &balances, nil, &interestClaims, nil, &actions, nil, nil, nil)
	return ContractTest{
		BlockHeight:   0,
		Contracts:     make(map[string][]byte),
		LedgerSession: se.LedgerExecutor.NewSession(0),
		State:         make(map[string]contract_execution_context.StateStore),
		StateEngine:   se,
	}
}

// Increment a specified number of L1 blocks in the contract testing environment.
func (ct *ContractTest) IncrementBlocks(count uint64) {
	currentSlot := ct.BlockHeight - (ct.BlockHeight % common.CONSENSUS_SPECS.SlotLength)
	newHeight := ct.BlockHeight + count
	newSlot := newHeight - (newHeight % common.CONSENSUS_SPECS.SlotLength)
	if newSlot > currentSlot {
		compiled := ct.StateEngine.LedgerExecutor.Compile(currentSlot)
		if compiled != nil {
			ct.executeLedgerOpLogs(compiled.OpLog, currentSlot, newSlot-1)
		}
		ct.StateEngine.UpdateBalances(currentSlot, newSlot-1)
		ct.LedgerSession = ct.StateEngine.LedgerExecutor.NewSession(newSlot)
	}
	ct.BlockHeight = newHeight
}

// Register a contract from bytecode.
func (ct *ContractTest) RegisterContract(contractId string, bytecode []byte) {
	ct.Contracts[contractId] = bytecode
}

// Executes a contract call transaction. Returns the call result, gas used and logs emitted.
func (ct *ContractTest) Call(tx stateEngine.TxVscCallContract) (stateEngine.TxResult, uint, []string) {
	bytecode, exists := ct.Contracts[tx.ContractId]
	if !exists {
		return stateEngine.TxResult{
			Success: false,
			Ret:     "contract not found",
			RcUsed:  100,
		}, 0, []string{}
	}

	w := wasm_runtime_ipc.New()
	w.Init()

	caller := ""
	if len(tx.Self.RequiredAuths) > 0 {
		caller = tx.Self.RequiredAuths[0]
	} else if len(tx.Self.RequiredPostingAuths) > 0 {
		caller = tx.Self.RequiredPostingAuths[0]
	}

	_, stateExists := ct.State[tx.ContractId]
	if !stateExists {
		ct.State[tx.ContractId] = NewInMemoryStateStore()
	}

	ctxValue := contract_execution_context.New(
		contract_execution_context.Environment{
			ContractId:           tx.ContractId,
			BlockHeight:          ct.BlockHeight,
			TxId:                 tx.Self.TxId,
			BlockId:              tx.Self.BlockId,
			Index:                tx.Self.Index,
			OpIndex:              tx.Self.OpIndex,
			Timestamp:            tx.Self.Timestamp,
			RequiredAuths:        tx.Self.RequiredAuths,
			RequiredPostingAuths: tx.Self.RequiredPostingAuths,
			Caller:               caller,
			Intents:              tx.Intents,
		},
		int64(tx.RcLimit), ct.LedgerSession, ct.State[tx.ContractId], contracts.ContractMetadata{},
	)
	ctx := context.WithValue(context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue), wasm_context.WasmExecCodeCtxKey, hex.EncodeToString(bytecode))
	res := w.Execute(ctx, tx.RcLimit*common.CYCLE_GAS_PER_RC, tx.Action, string(tx.Payload), wasm_runtime.Go)

	if res.Error != nil {
		ct.LedgerSession.Revert()
		ct.State[tx.ContractId].Rollback()
		return stateEngine.TxResult{
			Success: false,
			Err:     &res.ErrorCode,
			Ret:     *res.Error,
			RcUsed:  10,
		}, res.Result.Gas, []string{}
	}
	ct.LedgerSession.Done()
	ct.State[tx.ContractId].Commit()

	rcUsed := int64(math.Max(math.Ceil(float64(res.Result.Gas)/common.CYCLE_GAS_PER_RC), 100))

	return stateEngine.TxResult{
		Success: true,
		Ret:     res.Result.Result,
		RcUsed:  rcUsed,
	}, res.Result.Gas, ctxValue.Logs()
}

// Add funds to an account in the ledger.
func (ct *ContractTest) Deposit(toAccount string, amount int64, asset ledgerDb.Asset) {
	randomTxId := randomHex(40)
	ct.StateEngine.LedgerExecutor.Deposit(stateEngine.Deposit{
		Id:          randomTxId,
		BlockHeight: ct.BlockHeight,
		From:        "contract-test-account",
		Asset:       string(asset),
		Amount:      amount,
		Memo:        "&to=" + toAccount,
	})
}

// Retrieve the current balance of an account.
func (ct *ContractTest) GetBalance(account string, asset ledgerDb.Asset) int64 {
	return ct.LedgerSession.GetBalance(account, ct.BlockHeight, string(asset))
}

// Retrieve the balance of an account at the start of the slot.
func (ct *ContractTest) GetBalanceAtSlotStart(account string, asset ledgerDb.Asset) int64 {
	return ct.StateEngine.LedgerExecutor.Ls.GetBalance(account, ct.BlockHeight, string(asset))
}

// Set the value of a key in the contract state storage
func (ct *ContractTest) StateSet(contractId string, key string, value string) {
	if _, exists := ct.State[contractId]; !exists {
		ct.State[contractId] = NewInMemoryStateStore()
	}
	ct.State[contractId].Set(key, []byte(value))
	ct.State[contractId].Commit()
}

// Retrieve the value of a key from the contract state storage
func (ct *ContractTest) StateGet(contractId string, key string) string {
	if _, exists := ct.State[contractId]; !exists {
		return ""
	}
	return string(ct.State[contractId].Get(key))
}

// Unset the value of a key in the contract state storage
func (ct *ContractTest) StateDelete(contractId string, key string) {
	if _, exists := ct.State[contractId]; !exists {
		return
	}
	ct.State[contractId].Delete(key)
	ct.State[contractId].Commit()
}

func (ct *ContractTest) executeLedgerOpLogs(ledgerOps []ledgerSystem.OpLogEvent, startBlock uint64, endBlock uint64) {
	ct.StateEngine.LedgerExecutor.Flush()
	ct.StateEngine.Flush()

	aoplog := make([]ledgerSystem.OpLogEvent, 0)
	for _, v := range ledgerOps {
		v.BlockHeight = ct.BlockHeight
		aoplog = append(aoplog, v)
	}

	ct.StateEngine.LedgerExecutor.Ls.IngestOplog(aoplog, stateEngine.OplogInjestOptions{
		EndHeight:   endBlock,
		StartHeight: startBlock,
	})
}
