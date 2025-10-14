package test_utils

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"vsc-node/lib/datalayer"
	"vsc-node/lib/logger"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/witnesses"
	ledgerSystem "vsc-node/modules/ledger-system"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	wasm_context "vsc-node/modules/wasm/context"
	wasm_runtime "vsc-node/modules/wasm/runtime"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"

	"github.com/ipfs/go-cid"
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
	ContractDb    contracts.Contracts
	LedgerSession *stateEngine.LedgerSession
	CallSession   *contract_session.CallSession
	StateEngine   *stateEngine.StateEngine
	DataLayer     *datalayer.DataLayer
}

// Create a new contract testing environment with mock databases.
func NewContractTest() ContractTest {
	logr := logger.PrefixedLogger{Prefix: "contract-test"}
	idConfig := common.NewIdentityConfig()
	sysConfig := common.SystemConfig{
		Network: "mocknet",
	}
	ledgers := MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	balances := MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	interestClaims := MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	actions := MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	elections := MockElectionDb{}
	contractDb := MockContractDb{Contracts: make(map[string]contracts.Contract)}
	contractState := MockContractStateDb{Outputs: make(map[string]contracts.ContractOutput)}
	witnessesDb := witnesses.NewEmptyWitnesses()
	se := stateEngine.New(
		logr,
		nil,
		nil,
		&elections,
		&contractDb,
		nil,
		nil,
		&ledgers,
		&balances,
		nil,
		&interestClaims,
		nil,
		&actions,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
	)
	p2p := p2pInterface.New(witnessesDb, idConfig, sysConfig)
	dl := datalayer.New(p2p)
	a := aggregate.New([]aggregate.Plugin{idConfig, p2p, dl})
	if err := a.Init(); err != nil {
		panic(err)
	}
	return ContractTest{
		BlockHeight:   0,
		ContractDb:    &contractDb,
		LedgerSession: se.LedgerExecutor.NewSession(0),
		CallSession:   contract_session.NewCallSession(dl, &contractDb, &contractState, nil, 0),
		DataLayer:     dl,
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
func (ct *ContractTest) RegisterContract(contractId string, owner string, bytecode []byte) {
	cid, err := ct.DataLayer.PutRaw(bytecode, datalayer.PutRawOptions{Pin: true})
	if err != nil {
		panic(fmt.Errorf("failed to create cid for contract %s", contractId))
	}
	ct.ContractDb.RegisterContract(contractId, contracts.Contract{
		Id:             contractId,
		Owner:          owner,
		Code:           cid.String(),
		CreationHeight: ct.BlockHeight,
		Runtime:        wasm_runtime.Go,
	})
}

// Executes a contract call transaction. Returns the call result, gas used and logs emitted.
func (ct *ContractTest) Call(tx stateEngine.TxVscCallContract) (stateEngine.TxResult, uint, map[string][]string) {
	info, err := ct.ContractDb.ContractById(tx.ContractId)
	if err != nil {
		return stateEngine.TxResult{
			Success: false,
			Ret:     err.Error(),
			RcUsed:  100,
		}, 0, map[string][]string{}
	}

	c, err := cid.Decode(info.Code)
	if err != nil {
		return stateEngine.TxResult{
			Success: false,
			Ret:     err.Error(),
			RcUsed:  100,
		}, 0, map[string][]string{}
	}

	node, err := ct.DataLayer.Get(c, nil)
	if err != nil {
		return stateEngine.TxResult{
			Success: false,
			Ret:     err.Error(),
			RcUsed:  100,
		}, 0, map[string][]string{}
	}

	code := node.RawData()

	w := wasm_runtime_ipc.New()
	w.Init()

	caller := ""
	if len(tx.Self.RequiredAuths) > 0 {
		caller = tx.Self.RequiredAuths[0]
	} else if len(tx.Self.RequiredPostingAuths) > 0 {
		caller = tx.Self.RequiredPostingAuths[0]
	}

	ctxValue := contract_execution_context.New(
		contract_execution_context.Environment{
			ContractId:           tx.ContractId,
			ContractOwner:        info.Owner,
			BlockHeight:          ct.BlockHeight,
			TxId:                 tx.Self.TxId,
			BlockId:              tx.Self.BlockId,
			Index:                tx.Self.Index,
			OpIndex:              tx.Self.OpIndex,
			Timestamp:            tx.Self.Timestamp,
			RequiredAuths:        tx.Self.RequiredAuths,
			RequiredPostingAuths: tx.Self.RequiredPostingAuths,
			Caller:               caller,
			Sender:               caller,
			Intents:              tx.Intents,
		},
		int64(tx.RcLimit), tx.RcLimit*common.CYCLE_GAS_PER_RC, ct.LedgerSession, ct.CallSession, 0,
	)
	ctx := context.WithValue(
		context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue),
		wasm_context.WasmExecCodeCtxKey,
		hex.EncodeToString(code),
	)
	res := w.Execute(ctx, tx.RcLimit*common.CYCLE_GAS_PER_RC, tx.Action, string(tx.Payload), wasm_runtime.Go)
	rcUsed := int64(math.Max(math.Ceil(float64(res.Gas)/common.CYCLE_GAS_PER_RC), 100))

	if res.Error != nil {
		ct.LedgerSession.Revert()
		ct.CallSession.Rollback()
		return stateEngine.TxResult{
			Success: false,
			Err:     &res.ErrorCode,
			Ret:     *res.Error,
			RcUsed:  rcUsed,
		}, res.Gas, map[string][]string{}
	}
	ct.LedgerSession.Done()
	ct.CallSession.Commit()

	return stateEngine.TxResult{
		Success: true,
		Ret:     res.Result,
		RcUsed:  rcUsed,
	}, res.Gas, ct.CallSession.PopLogs()
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
	ct.CallSession.GetStateStore(contractId).Set(key, []byte(value))
	ct.CallSession.Commit()
}

// Retrieve the value of a key from the contract state storage
func (ct *ContractTest) StateGet(contractId string, key string) string {
	return string(ct.CallSession.GetStateStore(contractId).Get(key))
}

// Unset the value of a key in the contract state storage
func (ct *ContractTest) StateDelete(contractId string, key string) {
	ct.CallSession.GetStateStore(contractId).Delete(key)
	ct.CallSession.Commit()
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
