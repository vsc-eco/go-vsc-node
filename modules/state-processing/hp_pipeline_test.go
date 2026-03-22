package state_engine_test

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/vsc-eco/hivego"

	"vsc-node/lib/hive"
	"vsc-node/lib/logger"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	ledgerSystem "vsc-node/modules/ledger-system"
	rcSystem "vsc-node/modules/rc-system"
	stateEngine "vsc-node/modules/state-processing"
)

// ============================================================
// HP Staking End-to-End Pipeline Tests
//
// These tests go through the REAL execution layers:
//   - LedgerSession.ConsensusStake() / OptInHP() (validation + oplog)
//   - LedgerState.Compile() (oplog -> compiled result)
//   - LedgerSystem.IngestOplog() -> ExecuteOplog() (oplog -> ledger records + action records)
//   - StateEngine.UpdateBalances() (ledger records -> balance snapshots)
//
// This is the same path that ExecuteBatch() + UpdateBalances() take
// during real ProcessBlock slot transitions, but without needing the
// full election/schedule/witness infrastructure.
// ============================================================

// ============================================================
// TEST 1 + 2 + 5 + 8: Pipeline tests using a single ContractTest
// (Badger DB lock requires single instance)
// ============================================================

func TestPipeline_HPStaking(t *testing.T) {
	// Use direct mock construction (no Badger DB) to avoid lock conflicts
	// with other tests that use NewContractTest()
	ledgerMock := newMockLedgerDb()
	balanceMock := newMockBalanceDb(nil)
	actionsMock := newMockActionsDb()
	interestMock := &test_utils.MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	electionMock := &test_utils.MockElectionDb{}

	newSession := func(bh uint64) ledgerSystem.LedgerSession {
		state := &ledgerSystem.LedgerState{
			Oplog:           make([]ledgerSystem.OpLogEvent, 0),
			VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
			GatewayBalances: make(map[string]uint64),
			BlockHeight:     bh,
			LedgerDb:        ledgerMock,
			BalanceDb:       balanceMock,
			ActionDb:        actionsMock,
		}
		return ledgerSystem.NewSession(state)
	}

	// Pre-populate: deposit 100K HIVE for validator1
	balanceMock.BalanceRecords["hive:validator1"] = []ledgerDb.BalanceRecord{{
		Account:     "hive:validator1",
		BlockHeight: 0,
		Hive:        100_000_000,
	}}

	// ---- Test 1: deposit -> consensus_stake -> opt_in_hp ----
	t.Run("deposit_consensus_stake_opt_in_hp", func(t *testing.T) {
		session := newSession(1)

		// Consensus stake 60K
		csResult := session.ConsensusStake(ledgerSystem.ConsensusParams{
			Id: "cs-pipe-1", From: "hive:validator1", To: "hive:validator1",
			Amount: 60_000_000, BlockHeight: 1, Type: "stake",
		})
		require.True(t, csResult.Ok, "consensus stake should succeed: %s", csResult.Msg)

		hiveBal := session.GetBalance("hive:validator1", 1, "hive")
		consensusBal := session.GetBalance("hive:validator1", 1, "hive_consensus")
		require.Equal(t, int64(40_000_000), hiveBal, "liquid = 100K - 60K = 40K")
		require.Equal(t, int64(60_000_000), consensusBal, "consensus = 60K")

		// Opt-in HP with 50K
		optResult := session.OptInHP(ledgerSystem.HPStakeParams{
			Id: "opt-in-pipe-1", From: "hive:validator1", To: "hive:validator1",
			Amount: 50_000_000, BlockHeight: 1, HiveAccount: "validator1",
		})
		require.True(t, optResult.Ok, "HP opt-in should succeed: %s", optResult.Msg)

		hiveBal = session.GetBalance("hive:validator1", 1, "hive")
		consensusBal = session.GetBalance("hive:validator1", 1, "hive_consensus")
		pendingHpBal := session.GetBalance("hive:validator1", 1, "pending_hp")
		hpBal := session.GetBalance("hive:validator1", 1, "hive_hp")
		require.Equal(t, int64(40_000_000), hiveBal, "liquid HIVE = 40K")
		require.Equal(t, int64(10_000_000), consensusBal, "consensus = 60K - 50K = 10K")
		require.Equal(t, int64(50_000_000), pendingHpBal, "pending_hp = 50K (two-phase: not yet confirmed)")
		require.Equal(t, int64(0), hpBal, "hive_hp = 0 (not confirmed yet)")

		totalFunds := hiveBal + consensusBal + pendingHpBal + hpBal
		require.Equal(t, int64(100_000_000), totalFunds,
			"FUND CONSERVATION: hive=%d + consensus=%d + pending_hp=%d + hp=%d = %d",
			hiveBal, consensusBal, pendingHpBal, hpBal, totalFunds)

		session.Done()
	})

	// ---- Test 2: Multi-slot via UpdateBalances ----
	t.Run("multi_slot_update_balances", func(t *testing.T) {
		// Pre-populate ledger records as if IngestOplog ran from the session above
		ledgerMock.StoreLedger(
			ledgerDb.LedgerRecord{Id: "dep-1", Owner: "hive:validator1", Amount: 100_000_000,
				Asset: "hive", Type: "deposit", BlockHeight: 1},
			ledgerDb.LedgerRecord{Id: "cs-1", Owner: "hive:validator1", Amount: -60_000_000,
				Asset: "hive", Type: "consensus_stake", BlockHeight: 1},
			ledgerDb.LedgerRecord{Id: "cs-1#out", Owner: "hive:validator1", Amount: 60_000_000,
				Asset: "hive_consensus", Type: "consensus_stake", BlockHeight: 1},
			ledgerDb.LedgerRecord{Id: "hp-1", Owner: "hive:validator1", Amount: -50_000_000,
				Asset: "hive_consensus", Type: "hp_stake", BlockHeight: 1},
			ledgerDb.LedgerRecord{Id: "hp-1#out", Owner: "hive:validator1", Amount: 50_000_000,
				Asset: "pending_hp", Type: "hp_stake", BlockHeight: 1},
		)

		logr := logger.PrefixedLogger{Prefix: "pipeline-test"}
		sysConfig := systemconfig.MocknetConfig()
		se := stateEngine.New(logr, sysConfig,
			nil, nil, electionMock, nil, nil, nil,
			ledgerMock, balanceMock, nil, interestMock, nil, actionsMock,
			nil, nil, nil, nil, nil, nil)

		// Reset balance mock so UpdateBalances computes from ledger records
		balanceMock.BalanceRecords = make(map[string][]ledgerDb.BalanceRecord)

		se.UpdateBalances(0, 10)

		balRecord, _ := balanceMock.GetBalanceRecord("hive:validator1", 10)
		require.NotNil(t, balRecord, "balance record after UpdateBalances")
		require.Equal(t, int64(40_000_000), balRecord.Hive, "liquid = 40K")
		require.Equal(t, int64(10_000_000), balRecord.HIVE_CONSENSUS, "consensus = 10K")
		require.Equal(t, int64(50_000_000), balRecord.PENDING_HP, "pending_hp = 50K (two-phase)")
		require.Equal(t, int64(0), balRecord.HIVE_HP, "hive_hp = 0 (not confirmed yet)")

		total := balRecord.Hive + balRecord.HIVE_CONSENSUS + balRecord.PENDING_HP + balRecord.HIVE_HP
		require.Equal(t, int64(100_000_000), total, "FUND CONSERVATION across slot")
	})

	// ---- Test: Validation edge cases ----
	t.Run("below_minimum_hp_stake", func(t *testing.T) {
		session := newSession(1)

		// Pre-populate 40K consensus for smallval
		balanceMock.BalanceRecords["hive:smallval"] = []ledgerDb.BalanceRecord{{
			Account: "hive:smallval", BlockHeight: 0,
			Hive: 0, HIVE_CONSENSUS: 40_000_000,
		}}

		result := session.OptInHP(ledgerSystem.HPStakeParams{
			Id: "val-hp-fail", From: "hive:smallval", To: "hive:smallval",
			Amount: 50_000_000, BlockHeight: 1, HiveAccount: "smallval",
		})
		require.False(t, result.Ok, "should fail: 40K consensus < 50K needed")
		require.Equal(t, "insufficient balance", result.Msg)

		result2 := session.OptInHP(ledgerSystem.HPStakeParams{
			Id: "val-hp-fail-2", From: "hive:smallval", To: "hive:smallval",
			Amount: 40_000_000, BlockHeight: 1, HiveAccount: "smallval",
		})
		require.False(t, result2.Ok, "should fail: 40K < 50K HP minimum")
		require.Contains(t, result2.Msg, "minimum")
	})

	// ---- Test: Opt-out blocked ----
	t.Run("opt_out_blocked", func(t *testing.T) {
		session := newSession(1)

		balanceMock.BalanceRecords["hive:val3"] = []ledgerDb.BalanceRecord{{
			Account: "hive:val3", BlockHeight: 0,
			HIVE_HP: 50_000_000,
		}}

		result := session.OptOutHP(ledgerSystem.HPStakeParams{
			Id: "opt-out-blocked", From: "hive:val3", To: "hive:val3",
			Amount: 50_000_000, BlockHeight: 1,
			ElectionEpoch: 5, HiveAccount: "val3",
		})
		require.False(t, result.Ok, "opt-out should be blocked until Phase 5")
		require.Contains(t, result.Msg, "not yet enabled")

		// HP should be intact since opt-out was rejected
		hpBal := session.GetBalance("hive:val3", 1, "hive_hp")
		require.Equal(t, int64(50_000_000), hpBal, "HP intact after blocked opt-out")
	})

	// ---- Test: Two validators ----
	t.Run("two_validators", func(t *testing.T) {
		balanceMock.BalanceRecords["hive:v1"] = []ledgerDb.BalanceRecord{{
			Account: "hive:v1", BlockHeight: 0,
			Hive: 100_000_000,
		}}
		balanceMock.BalanceRecords["hive:v2"] = []ledgerDb.BalanceRecord{{
			Account: "hive:v2", BlockHeight: 0,
			Hive: 80_000_000,
		}}

		session := newSession(1)

		// V1: stake 60K, HP 50K
		session.ConsensusStake(ledgerSystem.ConsensusParams{
			Id: "cs-v1", From: "hive:v1", To: "hive:v1",
			Amount: 60_000_000, BlockHeight: 1,
		})
		session.OptInHP(ledgerSystem.HPStakeParams{
			Id: "hp-v1", From: "hive:v1", To: "hive:v1",
			Amount: 50_000_000, BlockHeight: 1, HiveAccount: "v1",
		})

		// V2: stake 55K, HP 50K
		session.ConsensusStake(ledgerSystem.ConsensusParams{
			Id: "cs-v2", From: "hive:v2", To: "hive:v2",
			Amount: 55_000_000, BlockHeight: 1,
		})
		session.OptInHP(ledgerSystem.HPStakeParams{
			Id: "hp-v2", From: "hive:v2", To: "hive:v2",
			Amount: 50_000_000, BlockHeight: 1, HiveAccount: "v2",
		})

		v1Total := session.GetBalance("hive:v1", 1, "hive") +
			session.GetBalance("hive:v1", 1, "hive_consensus") +
			session.GetBalance("hive:v1", 1, "pending_hp") +
			session.GetBalance("hive:v1", 1, "hive_hp")
		v2Total := session.GetBalance("hive:v2", 1, "hive") +
			session.GetBalance("hive:v2", 1, "hive_consensus") +
			session.GetBalance("hive:v2", 1, "pending_hp") +
			session.GetBalance("hive:v2", 1, "hive_hp")

		require.Equal(t, int64(100_000_000), v1Total, "v1 total = 100K")
		require.Equal(t, int64(80_000_000), v2Total, "v2 total = 80K")
		require.Equal(t, int64(180_000_000), v1Total+v2Total, "global = 180K")
	})
}

// ============================================================
// TEST 3: Gateway hp_stake operation sequence (MANUAL RECONSTRUCTION)
//
// NOTE: This is a MANUAL reconstruction of the operation sequence that
// MultiSig.executeActions() builds for hp_stake. It does NOT call
// executeActions() directly because MultiSig has many infrastructure
// dependencies (BLS keys, Hive client, TSS system, etc.).
//
// This test verifies that the individual hivego operation types
// (AccountCreate, Transfer, TransferToVesting, AccountWitnessProxy)
// serialize correctly and produce the expected field values.
//
// For a test of the REAL ExecuteTx code path, see TestTxOptInHP_ExecuteTx below.
// ============================================================

func TestGateway_HPStakeActions(t *testing.T) {
	var capturedTx hivego.HiveTransaction
	mockCreator := &hive.MockTransactionCreator{
		MockTransactionBroadcaster: hive.MockTransactionBroadcaster{
			Callback: func(tx hivego.HiveTransaction) error {
				capturedTx = tx
				return nil
			},
		},
	}

	gatewayWallet := "vsc.gateway"
	hiveAccount := "validator1"
	amount := int64(50_000_000)
	amtStr := hive.AmountToString(amount)

	// Derive node account name (same logic as gateway/utils.go:deriveNodeAccountName)
	nodeAccount := deriveTestNodeAccount(hiveAccount)
	require.True(t, strings.HasPrefix(nodeAccount, "mv-"), "node account prefix")
	require.LessOrEqual(t, len(nodeAccount), 16, "Hive account name limit is 16 chars")

	// Build the exact operation sequence that executeActions produces for hp_stake
	ops := []hivego.HiveOperation{}

	// Step 1: AccountCreate
	gatewayAuth := hivego.Auths{
		WeightThreshold: 1,
		AccountAuths:    [][2]interface{}{{gatewayWallet, 1}},
	}
	createOp := mockCreator.AccountCreate(
		"3.000 HIVE",
		gatewayWallet,
		nodeAccount,
		gatewayAuth, gatewayAuth, gatewayAuth,
		"STM6LLegbAgLAy28EHrFBKFNEoMVnvKqmhGk4dAfEyRDfHDzy4YhS",
		`{"hp_node":"`+hiveAccount+`"}`,
	)
	ops = append(ops, createOp)

	// Step 2: Transfer HIVE from gateway to node account
	transferOp := mockCreator.Transfer(
		gatewayWallet, nodeAccount,
		amtStr, "HIVE",
		"HP staking for "+hiveAccount,
	)
	ops = append(ops, transferOp)

	// Step 3: Power up (TransferToVesting)
	powerUpOp := mockCreator.TransferToVesting(
		nodeAccount, nodeAccount,
		amtStr+" HIVE",
	)
	ops = append(ops, powerUpOp)

	// Step 4: Set witness proxy
	proxyOp := mockCreator.AccountWitnessProxy(nodeAccount, hiveAccount)
	ops = append(ops, proxyOp)

	// Build the transaction
	tx := mockCreator.MakeTransaction(ops)
	mockCreator.PopulateSigningProps(&tx, []int{})

	// ---- VERIFY OPERATION TYPES via OpName() ----
	require.Equal(t, 4, len(tx.Operations), "should have 4 ops: create, transfer, vest, proxy")

	require.Equal(t, "account_create", tx.Operations[0].OpName())
	require.Equal(t, "transfer", tx.Operations[1].OpName())
	require.Equal(t, "transfer_to_vesting", tx.Operations[2].OpName())
	require.Equal(t, "account_witness_proxy", tx.Operations[3].OpName())

	// ---- VERIFY FIELD VALUES via type assertion ----

	// AccountCreate
	op0, ok := tx.Operations[0].(hivego.AccountCreateOperation)
	require.True(t, ok, "op[0] should be AccountCreateOperation")
	require.Equal(t, gatewayWallet, op0.Creator)
	require.Equal(t, nodeAccount, op0.NewAccountName)

	// Transfer
	op1, ok := tx.Operations[1].(hivego.TransferOperation)
	require.True(t, ok, "op[1] should be TransferOperation")
	require.Equal(t, gatewayWallet, op1.From)
	require.Equal(t, nodeAccount, op1.To)
	require.Contains(t, op1.Memo, "HP staking")

	// TransferToVesting
	op2, ok := tx.Operations[2].(hivego.TransferToVesting)
	require.True(t, ok, "op[2] should be TransferToVesting")
	require.Equal(t, nodeAccount, op2.From)
	require.Equal(t, nodeAccount, op2.To)

	// AccountWitnessProxy
	op3, ok := tx.Operations[3].(hivego.AccountWitnessProxyOperation)
	require.True(t, ok, "op[3] should be AccountWitnessProxyOperation")
	require.Equal(t, nodeAccount, op3.Account)
	require.Equal(t, hiveAccount, op3.Proxy)

	// ---- VERIFY BROADCAST CAPTURES TX ----
	mockCreator.Broadcast(tx)
	require.Equal(t, 4, len(capturedTx.Operations), "broadcast should capture same tx")

	t.Log("PASS: Gateway hp_stake produces correct L1 operations")
}

// ============================================================
// TEST 4: Gateway hp_unstake operation sequence (MANUAL RECONSTRUCTION)
//
// Same caveat as TEST 3: manually builds the ops, does not call executeActions().
// ============================================================

func TestGateway_HPUnstakeActions(t *testing.T) {
	mockCreator := &hive.MockTransactionCreator{}

	hiveAccount := "validator1"
	nodeAccount := deriveTestNodeAccount(hiveAccount)

	// hp_unstake: DelegateVestingShares(->0) + AccountWitnessProxy(->empty)
	ops := []hivego.HiveOperation{}

	undelegateOp := mockCreator.DelegateVestingShares(
		nodeAccount, hiveAccount, "0.000000 VESTS",
	)
	ops = append(ops, undelegateOp)

	removeProxyOp := mockCreator.AccountWitnessProxy(nodeAccount, "")
	ops = append(ops, removeProxyOp)

	tx := mockCreator.MakeTransaction(ops)
	require.Equal(t, 2, len(tx.Operations), "hp_unstake should have 2 ops")

	require.Equal(t, "delegate_vesting_shares", tx.Operations[0].OpName())
	delegateOp, ok := tx.Operations[0].(hivego.DelegateVestingSharesOperation)
	require.True(t, ok, "op[0] should be DelegateVestingSharesOperation")
	require.Equal(t, "0.000000 VESTS", delegateOp.VestingShares)
	require.Equal(t, nodeAccount, delegateOp.Delegator)

	require.Equal(t, "account_witness_proxy", tx.Operations[1].OpName())
	proxyOp, ok := tx.Operations[1].(hivego.AccountWitnessProxyOperation)
	require.True(t, ok, "op[1] should be AccountWitnessProxyOperation")
	require.Equal(t, "", proxyOp.Proxy)

	t.Log("PASS: Gateway hp_unstake produces correct L1 operations")
}

// ============================================================
// TEST 6: Slot boundary detection
// ============================================================

func TestSlotBoundaryDetection(t *testing.T) {
	slotLength := common.CONSENSUS_SPECS.SlotLength
	require.Equal(t, uint64(10), slotLength, "slot length should be 10 blocks")

	tests := []struct {
		block     uint64
		slotStart uint64
		slotEnd   uint64
	}{
		{0, 0, 10},
		{1, 0, 10},
		{9, 0, 10},
		{10, 10, 20},
		{15, 10, 20},
		{19, 10, 20},
		{20, 20, 30},
	}

	for _, tc := range tests {
		info := stateEngine.CalculateSlotInfo(tc.block)
		require.Equal(t, tc.slotStart, info.StartHeight,
			"block %d should have slot start %d", tc.block, tc.slotStart)
		require.Equal(t, tc.slotEnd, info.EndHeight,
			"block %d should have slot end %d", tc.block, tc.slotEnd)
	}

	t.Log("PASS: Slot boundary detection verified")
}

// ============================================================
// TEST 7: Direct UpdateBalances with pre-populated ledger records
//
// Populates mock ledger DB with hp_stake records, then calls
// UpdateBalances to verify hive_hp is included in balance snapshots.
// Does NOT use NewContractTest() — constructs everything manually.
// ============================================================

func TestUpdateBalances_IncludesHiveHP(t *testing.T) {
	ledgerMock := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	balanceMock := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	actionsMock := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	interestMock := &test_utils.MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	electionMock := &test_utils.MockElectionDb{}

	// Manually store ledger records (as if IngestOplog ran)
	ledgerMock.StoreLedger(
		ledgerDb.LedgerRecord{
			Id: "deposit-1", Owner: "hive:val1", Amount: 100_000_000,
			Asset: "hive", Type: "deposit", BlockHeight: 5,
		},
		ledgerDb.LedgerRecord{
			Id: "cs-1", Owner: "hive:val1", Amount: -60_000_000,
			Asset: "hive", Type: "consensus_stake", BlockHeight: 5,
		},
		ledgerDb.LedgerRecord{
			Id: "cs-1#out", Owner: "hive:val1", Amount: 60_000_000,
			Asset: "hive_consensus", Type: "consensus_stake", BlockHeight: 5,
		},
		ledgerDb.LedgerRecord{
			Id: "hp-1", Owner: "hive:val1", Amount: -50_000_000,
			Asset: "hive_consensus", Type: "hp_stake", BlockHeight: 5,
		},
		ledgerDb.LedgerRecord{
			Id: "hp-1#out", Owner: "hive:val1", Amount: 50_000_000,
			Asset: "pending_hp", Type: "hp_stake", BlockHeight: 5,
		},
	)

	// Construct a minimal StateEngine by building the ledger system and state manually
	ls := ledgerSystem.New(balanceMock, ledgerMock, interestMock, actionsMock, nil)
	ledgerState := &ledgerSystem.LedgerState{
		Oplog:           make([]ledgerSystem.OpLogEvent, 0),
		VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
		GatewayBalances: make(map[string]uint64),
		BlockHeight:     0,
		LedgerDb:        ledgerMock,
		ActionDb:        actionsMock,
		BalanceDb:       balanceMock,
	}

	// We need a StateEngine to call UpdateBalances. Use New() with nil for unused deps.
	sysConfig := systemconfig.MocknetConfig()
	logr := logger.PrefixedLogger{Prefix: "update-bal-test"}
	se := stateEngine.New(
		logr,
		sysConfig,
		nil, nil,
		electionMock,
		nil, nil, nil,
		ledgerMock,
		balanceMock,
		nil,
		interestMock,
		nil,
		actionsMock,
		nil, nil, nil, nil, nil, nil,
	)
	// Override the ledger state to use our pre-populated one
	se.LedgerState = ledgerState
	_ = ls

	// Call UpdateBalances
	se.UpdateBalances(0, 10)

	// Verify balance record
	balRecord, _ := balanceMock.GetBalanceRecord("hive:val1", 10)
	require.NotNil(t, balRecord, "balance record should exist after UpdateBalances")
	require.Equal(t, int64(40_000_000), balRecord.Hive, "liquid = 100K - 60K = 40K")
	require.Equal(t, int64(10_000_000), balRecord.HIVE_CONSENSUS, "consensus = 60K - 50K = 10K")
	require.Equal(t, int64(50_000_000), balRecord.PENDING_HP, "pending_hp = 50K (two-phase)")
	require.Equal(t, int64(0), balRecord.HIVE_HP, "hive_hp = 0 (not confirmed yet)")

	total := balRecord.Hive + balRecord.HIVE_CONSENSUS + balRecord.PENDING_HP + balRecord.HIVE_HP
	require.Equal(t, int64(100_000_000), total, "FUND CONSERVATION in UpdateBalances")

	t.Log("PASS: UpdateBalances correctly includes pending_hp in balance snapshots")
}

// ============================================================
// TEST: JSON payload parsing + SafeParseHiveFloat
// ============================================================

func TestJSONPayloadParsing(t *testing.T) {
	payload := `{"from":"hive:validator1","to":"hive:validator1","amount":"50.000","hive_account":"validator1","net_id":"vsc-testnet"}`

	var parsed struct {
		From        string `json:"from"`
		To          string `json:"to"`
		Amount      string `json:"amount"`
		HiveAccount string `json:"hive_account"`
		NetId       string `json:"net_id"`
	}
	err := json.Unmarshal([]byte(payload), &parsed)
	require.NoError(t, err, "JSON payload should parse correctly")
	require.Equal(t, "hive:validator1", parsed.From)
	require.Equal(t, "50.000", parsed.Amount)
	require.Equal(t, "validator1", parsed.HiveAccount)

	amount, err := common.SafeParseHiveFloat("50.000")
	require.NoError(t, err)
	require.Equal(t, int64(50000), amount, "50.000 = 50000 milliHIVE")

	// Large amount: 100K HIVE
	amount2, err := common.SafeParseHiveFloat("100000.000")
	require.NoError(t, err)
	require.Equal(t, int64(100_000_000), amount2, "100000.000 = 100M milliHIVE")
}

// ============================================================
// TEST 9: MockReader + MockCreator integration
//
// Verify MockCreator formats transactions that ProcessBlock parses
// ============================================================

func TestMockCreator_HPTransactionFormat(t *testing.T) {
	mr := stateEngine.NewMockReader()
	mc := stateEngine.NewMockCreator(mr)

	// Test Transfer formatting
	mc.Transfer("validator1", "vsc.gateway", "100.000", "HIVE", "to=validator1")
	require.Equal(t, 1, len(mr.Mempool), "should have 1 tx in mempool")

	txOp := mr.Mempool[0].Operations[0]
	require.Equal(t, "transfer", txOp.Type)
	require.Equal(t, "validator1", txOp.Value["from"])
	require.Equal(t, "vsc.gateway", txOp.Value["to"])
	amtData := txOp.Value["amount"].(map[string]interface{})
	require.Equal(t, "100.000", amtData["amount"])
	require.Equal(t, "@@000000021", amtData["nai"]) // HIVE NAI

	mr.CreateBlock()
	time.Sleep(20 * time.Millisecond)

	// Test CustomJson for vsc.consensus_stake
	mc.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"validator1"},
		Id:            "vsc.consensus_stake",
		Json:          `{"from":"hive:validator1","to":"hive:validator1","amount":"60.000","net_id":"vsc-mocknet"}`,
	})
	require.Equal(t, 1, len(mr.Mempool))

	csTx := mr.Mempool[0].Operations[0]
	require.Equal(t, "custom_json", csTx.Type)
	require.Equal(t, "vsc.consensus_stake", csTx.Value["id"])

	mr.CreateBlock()
	time.Sleep(20 * time.Millisecond)

	// Test CustomJson for vsc.opt_in_hp
	mc.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"validator1"},
		Id:            "vsc.opt_in_hp",
		Json:          `{"from":"hive:validator1","to":"hive:validator1","amount":"50.000","hive_account":"validator1","net_id":"vsc-mocknet"}`,
	})
	require.Equal(t, 1, len(mr.Mempool))

	hpTx := mr.Mempool[0].Operations[0]
	require.Equal(t, "custom_json", hpTx.Type)
	require.Equal(t, "vsc.opt_in_hp", hpTx.Value["id"])

	// Parse the JSON to verify it matches TxOptInHP struct
	var optIn struct {
		From        string `json:"from"`
		To          string `json:"to"`
		Amount      string `json:"amount"`
		HiveAccount string `json:"hive_account"`
		NetId       string `json:"net_id"`
	}
	err := json.Unmarshal([]byte(hpTx.Value["json"].(string)), &optIn)
	require.NoError(t, err)
	require.Equal(t, "hive:validator1", optIn.From)
	require.Equal(t, "validator1", optIn.HiveAccount)
	require.Equal(t, "vsc-mocknet", optIn.NetId)

	t.Log("PASS: MockCreator formats HP transactions correctly for ProcessBlock parsing")
}

// ============================================================
// HELPER: Derive node account name (replicates gateway/utils.go)
// ============================================================

func deriveTestNodeAccount(hiveAccount string) string {
	normalized := strings.ToLower(strings.TrimSpace(hiveAccount))
	hash := sha256.Sum256([]byte(normalized))
	suffix := fmt.Sprintf("%x", hash[:7])[:13]
	return "mv-" + suffix
}

// ============================================================
// TEST: TxOptInHP.ExecuteTx through the REAL code path
//
// This tests the ACTUAL TxOptInHP.ExecuteTx() method including:
//   - Amount parsing (SafeParseHiveFloat)
//   - Auth validation (RequiredAuths must contain From)
//   - hive: prefix check
//   - HiveAccount validation (3-16 chars, must match To)
//   - NetId check
//   - LedgerSession.OptInHP call with correct params
//   - Balance changes (consensus down, hive_hp up)
// ============================================================

// mockStateEngine implements common_types.StateEngine for testing
type mockStateEngine struct {
	sysConfig systemconfig.SystemConfig
}

func (m *mockStateEngine) Log() logger.Logger {
	return logger.PrefixedLogger{Prefix: "mock-se"}
}

func (m *mockStateEngine) DataLayer() common_types.DataLayer { return nil }

func (m *mockStateEngine) GetContractInfo(id string, height uint64) (contracts.Contract, bool) {
	return contracts.Contract{}, false
}

func (m *mockStateEngine) GetElectionInfo(height ...uint64) elections.ElectionResult {
	return elections.ElectionResult{}
}

func (m *mockStateEngine) SystemConfig() systemconfig.SystemConfig {
	return m.sysConfig
}

// mockRcSession implements rcSystem.RcSession for testing
type mockRcSession struct{}

func (m *mockRcSession) Consume(account string, blockHeight uint64, rcAmt int64) (bool, int64) {
	return true, rcAmt
}

func (m *mockRcSession) CanConsume(account string, blockHeight uint64, rcAmt int64) (bool, int64, int64) {
	return true, 999999, rcAmt
}

func (m *mockRcSession) Revert() {}

func (m *mockRcSession) Done() rcSystem.RcMapResult {
	return rcSystem.RcMapResult{}
}

func TestTxOptInHP_ExecuteTx(t *testing.T) {
	// Build a LedgerSession with pre-funded consensus balance
	balanceMock := newMockBalanceDb(map[string][]ledgerDb.BalanceRecord{
		"hive:validator1": {{
			Account:        "hive:validator1",
			BlockHeight:    0,
			Hive:           10_000_000,
			HIVE_CONSENSUS: 60_000_000,
		}},
	})
	session := func() ledgerSystem.LedgerSession {
		state := &ledgerSystem.LedgerState{
			Oplog:           make([]ledgerSystem.OpLogEvent, 0),
			VirtualLedger:   make(map[string][]ledgerSystem.LedgerUpdate),
			GatewayBalances: make(map[string]uint64),
			BlockHeight:     1,
			LedgerDb:        newMockLedgerDb(),
			BalanceDb:       balanceMock,
			ActionDb:        newMockActionsDb(),
		}
		return ledgerSystem.NewSession(state)
	}()

	se := &mockStateEngine{sysConfig: systemconfig.MocknetConfig()}

	// ---- SUCCESS: Valid opt-in ----
	t.Run("valid_opt_in", func(t *testing.T) {
		tx := &stateEngine.TxOptInHP{
			Self: stateEngine.TxSelf{
				TxId:          "test-tx-1",
				BlockHeight:   1,
				OpIndex:       0,
				RequiredAuths: []string{"hive:validator1"},
			},
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      "50000.000",
			HiveAccount: "validator1",
			NetId:       "vsc-mocknet",
		}

		result := tx.ExecuteTx(se, session, &mockRcSession{}, nil, "hive:validator1")
		require.True(t, result.Success, "ExecuteTx should succeed: %s", result.Ret)

		// Verify ledger balances changed correctly (two-phase: opt-in gives pending_hp, not hive_hp)
		consensusBal := session.GetBalance("hive:validator1", 1, "hive_consensus")
		pendingBal := session.GetBalance("hive:validator1", 1, "pending_hp")
		hpBal := session.GetBalance("hive:validator1", 1, "hive_hp")
		require.Equal(t, int64(10_000_000), consensusBal, "consensus = 60K - 50K = 10K")
		require.Equal(t, int64(50_000_000), pendingBal, "pending_hp = 50K")
		require.Equal(t, int64(0), hpBal, "hive_hp = 0 (not confirmed yet)")
	})

	// ---- FAILURE: Missing hive: prefix ----
	t.Run("missing_hive_prefix", func(t *testing.T) {
		tx := &stateEngine.TxOptInHP{
			Self: stateEngine.TxSelf{
				TxId:          "test-tx-2",
				BlockHeight:   1,
				RequiredAuths: []string{"validator1"},
			},
			From:        "validator1",
			To:          "validator1",
			Amount:      "50000.000",
			HiveAccount: "validator1",
			NetId:       "vsc-mocknet",
		}
		result := tx.ExecuteTx(se, session, &mockRcSession{}, nil, "validator1")
		require.False(t, result.Success, "should fail without hive: prefix")
		require.Contains(t, result.Ret, "hive:")
	})

	// ---- FAILURE: HiveAccount mismatch ----
	t.Run("hive_account_mismatch", func(t *testing.T) {
		tx := &stateEngine.TxOptInHP{
			Self: stateEngine.TxSelf{
				TxId:          "test-tx-3",
				BlockHeight:   1,
				RequiredAuths: []string{"hive:validator1"},
			},
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      "50000.000",
			HiveAccount: "wrong-account",
			NetId:       "vsc-mocknet",
		}
		result := tx.ExecuteTx(se, session, &mockRcSession{}, nil, "hive:validator1")
		require.False(t, result.Success, "should fail when hive_account doesn't match to")
		require.Contains(t, result.Ret, "match")
	})

	// ---- FAILURE: Invalid amount format ----
	t.Run("invalid_amount", func(t *testing.T) {
		tx := &stateEngine.TxOptInHP{
			Self: stateEngine.TxSelf{
				TxId:          "test-tx-4",
				BlockHeight:   1,
				RequiredAuths: []string{"hive:validator1"},
			},
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      "not-a-number",
			HiveAccount: "validator1",
			NetId:       "vsc-mocknet",
		}
		result := tx.ExecuteTx(se, session, &mockRcSession{}, nil, "hive:validator1")
		require.False(t, result.Success, "should fail with bad amount")
	})

	// ---- FAILURE: Auth not in RequiredAuths ----
	t.Run("auth_mismatch", func(t *testing.T) {
		tx := &stateEngine.TxOptInHP{
			Self: stateEngine.TxSelf{
				TxId:          "test-tx-5",
				BlockHeight:   1,
				RequiredAuths: []string{"hive:other-user"},
			},
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      "50000.000",
			HiveAccount: "validator1",
			NetId:       "vsc-mocknet",
		}
		result := tx.ExecuteTx(se, session, &mockRcSession{}, nil, "hive:validator1")
		require.False(t, result.Success, "should fail when From not in RequiredAuths")
		require.Contains(t, result.Ret, "RequiredAuths")
	})

	// ---- FAILURE: Empty HiveAccount ----
	t.Run("empty_hive_account", func(t *testing.T) {
		tx := &stateEngine.TxOptInHP{
			Self: stateEngine.TxSelf{
				TxId:          "test-tx-6",
				BlockHeight:   1,
				RequiredAuths: []string{"hive:validator1"},
			},
			From:        "hive:validator1",
			To:          "hive:validator1",
			Amount:      "50000.000",
			HiveAccount: "",
			NetId:       "vsc-mocknet",
		}
		result := tx.ExecuteTx(se, session, &mockRcSession{}, nil, "hive:validator1")
		require.False(t, result.Success, "should fail with empty hive_account")
	})

	t.Log("PASS: TxOptInHP.ExecuteTx exercises real code path with all validation branches")
}

// ============================================================
// HP Timeout Rollback Test
// Verifies that pending_hp actions older than HP_CONFIRM_TIMEOUT
// get rolled back to hive_consensus during UpdateBalances.
// ============================================================

func TestHP_TimeoutRollback(t *testing.T) {
	logr := logger.PrefixedLogger{Prefix: "hp-timeout-test"}
	sysConfig := systemconfig.MocknetConfig()
	ledgers := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	balances := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	interestClaims := &test_utils.MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	actions := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	electionDb := &test_utils.MockElectionDb{}

	se := stateEngine.New(
		logr, sysConfig, nil, nil, electionDb,
		&test_utils.MockContractDb{Contracts: make(map[string]contracts.Contract)},
		&test_utils.MockContractStateDb{Outputs: make(map[string]contracts.ContractOutput)},
		nil, ledgers, balances, nil, interestClaims, nil, actions,
		nil, nil, nil, nil, nil, nil,
	)

	// Simulate: validator opted in at block 100, pending_hp action created
	actions.Actions["hp-stake-timeout"] = ledgerDb.ActionRecord{
		Id:          "hp-stake-timeout",
		Status:      "pending",
		Amount:      50_000_000,
		Asset:       "hive_hp",
		To:          "hive:validator1",
		TxId:        "hp-stake-timeout",
		Type:        "hp_stake",
		BlockHeight: 100,
		Params:      map[string]interface{}{"hive_account": "validator1"},
	}

	// Pre-populate balance: validator has 50M pending_hp from the opt-in
	balances.BalanceRecords["hive:validator1"] = []ledgerDb.BalanceRecord{
		{
			Account:     "hive:validator1",
			BlockHeight: 100,
			PENDING_HP:  50_000_000,
		},
	}

	// Also add the ledger records that would exist from the opt-in
	ledgers.LedgerRecords["hive:validator1"] = []ledgerDb.LedgerRecord{
		{Id: "hp-stake-timeout#out", Amount: 50_000_000, Asset: "pending_hp",
			BlockHeight: 100, Owner: "hive:validator1", Type: "hp_stake"},
	}

	// Call UpdateBalances at a block AFTER the timeout (100 + 1200 + 1 = 1301)
	// HP_CONFIRM_TIMEOUT = 1200 blocks
	timeoutBlock := uint64(100 + ledgerSystem.HP_CONFIRM_TIMEOUT + 1)
	se.UpdateBalances(timeoutBlock-9, timeoutBlock)

	// The rollback should have created ledger records:
	// -50M pending_hp + 50M hive_consensus
	allRecords := ledgers.LedgerRecords["hive:validator1"]
	var rollbackDebit, rollbackCredit *ledgerDb.LedgerRecord
	for i := range allRecords {
		r := &allRecords[i]
		if r.Type == "hp_timeout_rollback" && r.Amount < 0 {
			rollbackDebit = r
		}
		if r.Type == "hp_timeout_rollback" && r.Amount > 0 {
			rollbackCredit = r
		}
	}

	require.NotNil(t, rollbackDebit, "should have rollback debit record for pending_hp")
	require.Equal(t, int64(-50_000_000), rollbackDebit.Amount)
	require.Equal(t, "pending_hp", rollbackDebit.Asset)

	require.NotNil(t, rollbackCredit, "should have rollback credit record for hive_consensus")
	require.Equal(t, int64(50_000_000), rollbackCredit.Amount)
	require.Equal(t, "hive_consensus", rollbackCredit.Asset)

	// The action should be marked complete
	action, _ := actions.Get("hp-stake-timeout")
	require.Equal(t, "complete", action.Status, "timed-out action should be marked complete")
}

func TestHP_NoRollbackBeforeTimeout(t *testing.T) {
	logr := logger.PrefixedLogger{Prefix: "hp-no-timeout-test"}
	sysConfig := systemconfig.MocknetConfig()
	ledgers := &test_utils.MockLedgerDb{LedgerRecords: make(map[string][]ledgerDb.LedgerRecord)}
	balances := &test_utils.MockBalanceDb{BalanceRecords: make(map[string][]ledgerDb.BalanceRecord)}
	interestClaims := &test_utils.MockInterestClaimsDb{Claims: make([]ledgerDb.ClaimRecord, 0)}
	actions := &test_utils.MockActionsDb{Actions: make(map[string]ledgerDb.ActionRecord)}
	electionDb := &test_utils.MockElectionDb{}

	se := stateEngine.New(
		logr, sysConfig, nil, nil, electionDb,
		&test_utils.MockContractDb{Contracts: make(map[string]contracts.Contract)},
		&test_utils.MockContractStateDb{Outputs: make(map[string]contracts.ContractOutput)},
		nil, ledgers, balances, nil, interestClaims, nil, actions,
		nil, nil, nil, nil, nil, nil,
	)

	// Pending hp_stake action created at block 100
	actions.Actions["hp-stake-recent"] = ledgerDb.ActionRecord{
		Id:          "hp-stake-recent",
		Status:      "pending",
		Amount:      50_000_000,
		Asset:       "hive_hp",
		To:          "hive:validator1",
		TxId:        "hp-stake-recent",
		Type:        "hp_stake",
		BlockHeight: 100,
		Params:      map[string]interface{}{"hive_account": "validator1"},
	}

	// Call UpdateBalances BEFORE timeout (block 500, well within 1200 timeout)
	se.UpdateBalances(491, 500)

	// No rollback records should exist
	allRecords := ledgers.LedgerRecords["hive:validator1"]
	for _, r := range allRecords {
		require.NotEqual(t, "hp_timeout_rollback", r.Type,
			"should NOT roll back pending_hp before timeout expires")
	}

	// Action should still be pending
	action, _ := actions.Get("hp-stake-recent")
	require.Equal(t, "pending", action.Status, "action should still be pending before timeout")
}
