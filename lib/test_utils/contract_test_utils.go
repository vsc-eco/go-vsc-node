package test_utils

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"math"
	"vsc-node/lib/datalayer"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	contract_execution_context "vsc-node/modules/contract/execution-context"
	contract_session "vsc-node/modules/contract/session"
	"vsc-node/modules/db/vsc/contracts"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	rcDb "vsc-node/modules/db/vsc/rcs"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/witnesses"
	ledgerSystem "vsc-node/modules/ledger-system"
	p2pInterface "vsc-node/modules/p2p"
	rc_system "vsc-node/modules/rc-system"
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

type TssState struct {
	Keys        *MockTssKeysDb
	Commitments *MockTssCommitmentsDb
	Requests    *MockTssRequestsDb
}

// Contract testing environment
type ContractTest struct {
	BlockHeight   uint64
	ContractDb    contracts.Contracts
	LedgerSession ledgerSystem.LedgerSession
	RcSession     rc_system.RcSession
	RcDb          *MockRcDb
	CallSession   *contract_session.CallSession
	StateEngine   *stateEngine.StateEngine
	DataLayer     *datalayer.DataLayer
	Tss           TssState
}

type ContractTestCallResult struct {
	Success   bool
	Err       contracts.ContractOutputError
	ErrMsg    string
	Ret       string
	RcUsed    int64
	GasUsed   uint
	Logs      map[string]contract_session.LogOutput
	StateDiff map[string]contract_session.StateDiff
}

// Create a new contract testing environment with mock databases.
func NewContractTest() ContractTest {
	idConfig := common.NewIdentityConfig()
	p2pConfig := p2pInterface.NewConfig()
	p2pConfig.SetOptions(p2pInterface.P2POpts{Port: 0}) // random port to avoid conflicts with running nodes
	sysConfig := systemconfig.MocknetConfig()
	ledgers := MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	balances := MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	rc := MockRcDb{Records: make(map[string][]rcDb.RcRecord)}
	interestClaims := MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	actions := MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	elections := MockElectionDb{}
	contractDb := MockContractDb{Contracts: make(map[string]contracts.Contract)}
	contractState := MockContractStateDb{Outputs: make(map[string]contracts.ContractOutput)}
	witnessesDb := witnesses.NewEmptyWitnesses()

	tssKeys := MockTssKeysDb{Keys: make(map[string]tss_db.TssKey)}
	tssCommitments := MockTssCommitmentsDb{Commitments: make(map[string]tss_db.TssCommitment)}
	tssRequests := MockTssRequestsDb{Requests: make(map[string]tss_db.TssRequest)}

	se := stateEngine.New(
		sysConfig,
		nil,
		nil,
		&elections,
		&contractDb,
		&contractState,
		nil,
		&ledgers,
		&balances,
		nil,
		&interestClaims,
		nil,
		&actions,
		&rc,
		nil,
		&tssKeys,
		&tssCommitments,
		&tssRequests,
		nil,
	)
	var blockStatus common_types.BlockStatusGetter
	p2p := p2pInterface.New(witnessesDb, p2pConfig, idConfig, sysConfig, blockStatus)
	dl := datalayer.New(p2p)
	a := aggregate.New([]aggregate.Plugin{idConfig, p2pConfig, p2p, dl})
	if err := a.Init(); err != nil {
		panic(err)
	}

	state := se.LedgerSystem.NewEmptyState()
	ledgerSession := se.LedgerSystem.NewEmptySession(state, 0)
	return ContractTest{
		BlockHeight:   0,
		ContractDb:    &contractDb,
		LedgerSession: ledgerSession,
		RcSession:     se.RcSystem.NewSession(ledgerSession),
		RcDb:          &rc,
		CallSession:   contract_session.NewCallSession(dl, &contractDb, &contractState, &tssKeys, 0, nil),
		DataLayer:     dl,
		StateEngine:   se,
		Tss: TssState{
			Keys:        &tssKeys,
			Commitments: &tssCommitments,
			Requests:    &tssRequests,
		},
	}
}

// Increment a specified number of L1 blocks in the contract testing environment.
func (ct *ContractTest) IncrementBlocks(count uint64) {
	currentSlot := ct.BlockHeight - (ct.BlockHeight % common.CONSENSUS_SPECS.SlotLength)
	newHeight := ct.BlockHeight + count
	newSlot := newHeight - (newHeight % common.CONSENSUS_SPECS.SlotLength)
	if newSlot > currentSlot {
		compiled := ct.StateEngine.LedgerState.Compile(currentSlot)
		if compiled != nil {
			ct.executeLedgerOpLogs(compiled.OpLog, currentSlot, newSlot-1)
		}
		ct.StateEngine.UpdateBalances(currentSlot, newSlot-1)
		ct.StateEngine.UpdateRcMap(newSlot - 1)
		ct.StateEngine.RcMap = make(map[string]int64)

		ct.LedgerSession = ledgerSystem.NewSession(&ledgerSystem.LedgerState{
			Oplog:           make([]ledgerSystem.OpLogEvent, 0),
			VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
			GatewayBalances: make(map[string]uint64),

			BlockHeight: newSlot,
			LedgerDb:    ct.StateEngine.LedgerState.LedgerDb,
			BalanceDb:   ct.StateEngine.LedgerState.BalanceDb,
			ActionDb:    ct.StateEngine.LedgerState.ActionDb,
		})
		ct.RcSession = ct.StateEngine.RcSystem.NewSession(ct.LedgerSession)
		// ct.LedgerSession = ct.StateEngine.LedgerExecutor.NewSession(newSlot)
	}
	ct.BlockHeight = newHeight
}

// Register a contract from bytecode.
func (ct *ContractTest) RegisterContract(contractId string, owner string, bytecode []byte) {
	cid, err := ct.DataLayer.PutRaw(bytecode, common_types.PutRawOptions{Pin: true})
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
func (ct *ContractTest) Call(tx stateEngine.TxVscCallContract) ContractTestCallResult {
	info, err := ct.ContractDb.ContractById(tx.ContractId, tx.Self.BlockHeight)
	if err != nil {
		return ContractTestCallResult{
			Success: false,
			ErrMsg:  err.Error(),
			RcUsed:  100,
			GasUsed: 0,
			Logs:    map[string]contract_session.LogOutput{},
		}
	}

	c, err := cid.Decode(info.Code)
	if err != nil {
		return ContractTestCallResult{
			Success: false,
			ErrMsg:  err.Error(),
			RcUsed:  100,
			GasUsed: 0,
			Logs:    map[string]contract_session.LogOutput{},
		}
	}

	node, err := ct.DataLayer.Get(c, nil)
	if err != nil {
		return ContractTestCallResult{
			Success: false,
			ErrMsg:  err.Error(),
			RcUsed:  100,
			GasUsed: 0,
			Logs:    map[string]contract_session.LogOutput{},
		}
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
	rcPayer := caller

	hasMinRCs, availableRCs, _ := ct.RcSession.CanConsume(rcPayer, ct.BlockHeight, 100)
	if !hasMinRCs {
		return ContractTestCallResult{
			Success: false,
			ErrMsg:  fmt.Sprintf("minimum RC requirement is not met. RCs available: %d", availableRCs),
			RcUsed:  100,
			GasUsed: 0,
			Logs:    map[string]contract_session.LogOutput{},
		}
	}

	gas := min(uint(availableRCs), tx.RcLimit)
	const maxGas = ^uint(0) / params.CYCLE_GAS_PER_RC
	if gas > maxGas {
		gas = maxGas
	}

	// fmt.Println("tx intents:", tx.Intents)

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
		int64(gas), gas*params.CYCLE_GAS_PER_RC, ct.LedgerSession, ct.CallSession, 0,
	)
	ctx := context.WithValue(
		context.WithValue(context.Background(), wasm_context.WasmExecCtxKey, ctxValue),
		wasm_context.WasmExecCodeCtxKey,
		hex.EncodeToString(code),
	)
	res := w.Execute(ctx, gas*params.CYCLE_GAS_PER_RC, tx.Action, string(tx.Payload), wasm_runtime.Go)
	rcUsed := int64(math.Max(math.Ceil(float64(res.Gas)/params.CYCLE_GAS_PER_RC), 100))
	ct.RcSession.Consume(rcPayer, ct.BlockHeight, rcUsed)
	ct.StateEngine.RcMap[rcPayer] = ct.StateEngine.RcMap[rcPayer] + rcUsed

	if res.Error != nil {
		ct.LedgerSession.Revert()
		ct.CallSession.Rollback()
		return ContractTestCallResult{
			Success: false,
			Err:     res.ErrorCode,
			ErrMsg:  *res.Error,
			RcUsed:  rcUsed,
			GasUsed: res.Gas,
			Logs:    map[string]contract_session.LogOutput{},
		}
	}
	diff := ct.CallSession.GetStateDiff()
	ct.LedgerSession.Done()
	ct.CallSession.Commit()

	return ContractTestCallResult{
		Success:   true,
		Ret:       res.Result,
		RcUsed:    rcUsed,
		GasUsed:   res.Gas,
		Logs:      ct.CallSession.PopLogs(),
		StateDiff: diff,
	}
}

// Add funds to an account in the ledger.
func (ct *ContractTest) Deposit(toAccount string, amount int64, asset ledgerDb.Asset) {
	randomTxId := randomHex(40)
	ct.StateEngine.LedgerSystem.Deposit(ledgerSystem.Deposit{
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
	return ct.StateEngine.LedgerSystem.GetBalance(account, ct.BlockHeight, string(asset))
}

// Retrieve the total RCs available to an account at the current block height.
func (ct *ContractTest) GetAvailableRCs(account string) int64 {
	return ct.StateEngine.RcSystem.GetAvailableRCs(account, ct.BlockHeight)
}

// Set a frozen RC record for an account (simulates prior RC consumption thawing over time).
func (ct *ContractTest) SetFrozenRc(account string, blockHeight uint64, amount int64) {
	ct.RcDb.SetRecord(account, blockHeight, amount)
}

// Return the RC consumption map for the current RcSession.
func (ct *ContractTest) RcUsed() map[string]int64 {
	return ct.RcSession.Done().RcMap
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

// Set the value of a key in the ephemeral contract state
func (ct *ContractTest) EphemStateSet(contractId string, key string, value string) {
	ct.CallSession.GetStateStore(contractId).SetEphem(key, []byte(value))
}

// Retrieve the value of a key from the ephemeral contract state
func (ct *ContractTest) EphemStateGet(contractId string, key string) string {
	return string(ct.CallSession.GetStateStore(contractId).GetEphem(key))
}

// Unset the value of a key in the ephemeral contract state
func (ct *ContractTest) EphemStateDelete(contractId string, key string) {
	ct.CallSession.GetStateStore(contractId).DeleteEphem(key)
}

// Clear the ephemeral state of the contract if contract ID specified, or entire call session otherwise
func (ct *ContractTest) EphemStateClear(contractId ...string) {
	ct.CallSession.ClearEphemState(contractId...)
}

func (ct *ContractTest) executeLedgerOpLogs(ledgerOps []ledgerSystem.OpLogEvent, startBlock uint64, endBlock uint64) {
	ct.StateEngine.LedgerState.Flush()
	ct.StateEngine.Flush()

	aoplog := make([]ledgerSystem.OpLogEvent, 0)
	for _, v := range ledgerOps {
		v.BlockHeight = ct.BlockHeight
		aoplog = append(aoplog, v)
	}

	ct.StateEngine.LedgerSystem.IngestOplog(aoplog, ledgerSystem.OplogInjestOptions{
		EndHeight:   endBlock,
		StartHeight: startBlock,
	})
}
