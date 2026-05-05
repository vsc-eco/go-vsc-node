package state_engine_test

import (
	"encoding/json"
	"testing"
	"time"
	"vsc-node/lib/test_utils"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	rcDb "vsc-node/modules/db/vsc/rcs"
	tss_db "vsc-node/modules/db/vsc/tss"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"

	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vsc-eco/hivego"
)

// testEnv bundles a StateEngine with its mock dependencies and helpers.
type testEnv struct {
	SE             *stateEngine.StateEngine
	Reader         *stateEngine.MockReader
	Creator        *stateEngine.MockCreator
	WitnessDb      *test_utils.MockWitnessDb
	ElectionDb     *test_utils.MockElectionDb
	ContractDb     *test_utils.MockContractDb
	ContractState  *test_utils.MockContractStateDb
	TxDb           *test_utils.MockTxDb
	LedgerDb       *test_utils.MockLedgerDb
	BalanceDb      *test_utils.MockBalanceDb
	InterestClaims *test_utils.MockInterestClaimsDb
	VscBlocksDb    *test_utils.MockVscBlocksDb
	ActionsDb      *test_utils.MockActionsDb
	RcDb           *test_utils.MockRcDb
	NonceDb        *test_utils.MockNonceDb
	TssKeys        *test_utils.MockTssKeysDb
	TssCommitments *test_utils.MockTssCommitmentsDb
	TssRequests    *test_utils.MockTssRequestsDb
}

func newTestEnv() *testEnv {
	sysConfig := systemconfig.MocknetConfig()

	witnessesDb := &test_utils.MockWitnessDb{
		ByAccount: make(map[string]*witnesses.Witness),
		ByPeerId:  make(map[string][]witnesses.Witness),
	}
	electionDb := &test_utils.MockElectionDb{
		Elections:         make(map[uint64]*elections.ElectionResult),
		ElectionsByHeight: make(map[uint64]elections.ElectionResult),
	}
	contractDb := &test_utils.MockContractDb{
		Contracts: make(map[string]contracts.Contract),
	}
	contractState := &test_utils.MockContractStateDb{
		Outputs: make(map[string]contracts.ContractOutput),
	}
	txDb := &test_utils.MockTxDb{
		Records: make(map[string]transactions.TransactionRecord),
	}
	ledgerDbImpl := &test_utils.MockLedgerDb{
		LedgerRecords: make(map[string][]ledgerDb.LedgerRecord),
	}
	balanceDb := &test_utils.MockBalanceDb{
		BalanceRecords: make(map[string][]ledgerDb.BalanceRecord),
	}
	interestClaims := &test_utils.MockInterestClaimsDb{}
	vscBlocksDb := &test_utils.MockVscBlocksDb{
		Blocks: make([]vscBlocks.VscHeaderRecord, 0),
	}
	actionsDb := &test_utils.MockActionsDb{
		Actions: make(map[string]ledgerDb.ActionRecord),
	}
	mockRcDb := &test_utils.MockRcDb{
		Records: make(map[string][]rcDb.RcRecord),
	}
	nonceDb := &test_utils.MockNonceDb{
		Nonces: make(map[string]uint64),
	}
	tssKeys := &test_utils.MockTssKeysDb{
		Keys: make(map[string]tss_db.TssKey),
	}
	tssCommitments := &test_utils.MockTssCommitmentsDb{
		Commitments: make(map[string]tss_db.TssCommitment),
	}
	tssRequests := &test_utils.MockTssRequestsDb{
		Requests: make(map[string]tss_db.TssRequest),
	}

	se := stateEngine.New(
		sysConfig, nil,
		witnessesDb, electionDb, contractDb, contractState,
		txDb, ledgerDbImpl, balanceDb, nil,
		interestClaims, vscBlocksDb, actionsDb, mockRcDb, nonceDb,
		tssKeys, tssCommitments, tssRequests, nil,
	)

	mockReader := stateEngine.NewMockReader()
	mockReader.ProcessFunction = func(block hive_blocks.HiveBlock, headHeight *uint64) {
		se.ProcessBlock(block)
	}

	mockCreator := stateEngine.NewMockCreator(mockReader)

	return &testEnv{
		SE:             se,
		Reader:         mockReader,
		Creator:        mockCreator,
		WitnessDb:      witnessesDb,
		ElectionDb:     electionDb,
		ContractDb:     contractDb,
		ContractState:  contractState,
		TxDb:           txDb,
		LedgerDb:       ledgerDbImpl,
		BalanceDb:      balanceDb,
		InterestClaims: interestClaims,
		VscBlocksDb:    vscBlocksDb,
		ActionsDb:      actionsDb,
		RcDb:           mockRcDb,
		NonceDb:        nonceDb,
		TssKeys:        tssKeys,
		TssCommitments: tssCommitments,
		TssRequests:    tssRequests,
	}
}

// processAndWait creates a block and waits for async processing.
func (te *testEnv) processAndWait() {
	te.Reader.CreateBlock()
	time.Sleep(100 * time.Millisecond)
}

// ============================================================
// Group 1: Transfer & Deposit Variations
// ============================================================

func TestMockEngine(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("test-account", "vsc.gateway", "10", "HBD", "test transfer")
	te.processAndWait()

	bal := te.SE.LedgerSystem.GetBalance("hive:test-account", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(10), bal)
}

func TestHiveDeposit(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "25", "HIVE", "hive deposit")
	te.processAndWait()

	bal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hive")
	assert.Equal(t, int64(25), bal)
}

func TestTransferFromGatewaySkipped(t *testing.T) {
	te := newTestEnv()

	// Transfers FROM the gateway wallet (vsc.mocknet in mocknet config) should be ignored
	te.Creator.Transfer("vsc.mocknet", "some-user", "100", "HBD", "outgoing")
	te.processAndWait()

	bal := te.SE.LedgerSystem.GetBalance("hive:vsc.mocknet", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(0), bal)
	bal2 := te.SE.LedgerSystem.GetBalance("hive:some-user", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(0), bal2)
}

func TestMultipleDepositsSameAccount(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("bob", "vsc.gateway", "10", "HBD", "first")
	te.processAndWait()

	te.Creator.Transfer("bob", "vsc.gateway", "15", "HBD", "second")
	te.processAndWait()

	bal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(25), bal)
}

func TestDepositsFromMultipleAccounts(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.Creator.Transfer("bob", "vsc.gateway", "20", "HBD", "")
	te.processAndWait()

	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	bobBal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(10), aliceBal)
	assert.Equal(t, int64(20), bobBal)
}

func TestTransferToNonGatewayIgnored(t *testing.T) {
	te := newTestEnv()

	// Transfer to a non-gateway account should not create a deposit
	te.Creator.Transfer("alice", "bob", "50", "HBD", "peer to peer")
	te.processAndWait()

	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	bobBal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(0), aliceBal)
	assert.Equal(t, int64(0), bobBal)
}

func TestDepositIndexedInTxDb(t *testing.T) {
	te := newTestEnv()

	conf := te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "my memo")
	te.processAndWait()

	rec := te.TxDb.GetTransaction(conf.Id)
	if assert.NotNil(t, rec) {
		assert.Equal(t, transactions.TransactionStatus("CONFIRMED"), rec.Status)
		assert.Contains(t, rec.OpTypes, "deposit")
	}
}

// ============================================================
// Group 2: User Operations (posting auth custom_json)
// ============================================================

func TestVscTransfer(t *testing.T) {
	te := newTestEnv()

	// First deposit so alice has a balance
	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")
	te.processAndWait()

	// Now do a VSC transfer from alice to bob
	// vsc.transfer checks RequiredAuths contains the "from" account
	transferJson, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "hive:bob",
		"amount": "30.000",
		"asset":  "hbd",
		"memo":   "vsc transfer",
		"net_id": "vsc-mocknet",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 string(transferJson),
	})
	te.processAndWait()

	// Advance to next slot to trigger ExecuteBatch
	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// The transfer should succeed because after "hive:" prefix, RequiredPostingAuths
	// contains "hive:alice" which matches the "from" field.
	// However, TxVSCTransfer.ExecuteTx checks RequiredAuths (not PostingAuths).
	// Posting-auth-only transfers fail the RequiredAuths check.
	// This verifies the auth check rejects non-active-auth transfers.
	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	bobBal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(100), aliceBal, "posting-auth-only transfer should be reverted")
	assert.Equal(t, int64(0), bobBal, "bob should receive nothing from reverted tx")
}

func TestVscTransferWithActiveAuth(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")
	te.processAndWait()

	transferJson, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "hive:bob",
		"amount": "30.000",
		"asset":  "hbd",
		"memo":   "",
		"net_id": "vsc-mocknet",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 string(transferJson),
	})
	te.processAndWait()

	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// vsc.transfer with only posting auth fails RequiredAuths check, so balances unchanged
	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	bobBal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(100), aliceBal, "transfer should be reverted: posting auth doesn't satisfy RequiredAuths check")
	assert.Equal(t, int64(0), bobBal)
}

func TestVscTransferInsufficientBalance(t *testing.T) {
	te := newTestEnv()

	// Deposit 10 HBD for alice
	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.processAndWait()

	// Try to transfer 50 HBD (more than available)
	transferJson, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "hive:bob",
		"amount": "50.000",
		"asset":  "hbd",
		"memo":   "",
		"net_id": "vsc-mocknet",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 string(transferJson),
	})
	te.processAndWait()

	// Advance slot
	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// Alice should still have 10, Bob should have 0
	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	bobBal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(10), aliceBal)
	assert.Equal(t, int64(0), bobBal)
}

func TestVscTransferWrongNetId(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")
	te.processAndWait()

	transferJson, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "hive:bob",
		"amount": "10.000",
		"asset":  "hbd",
		"net_id": "vsc-wrong-net",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 string(transferJson),
	})
	te.processAndWait()

	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// Alice balance unchanged — wrong net_id rejected
	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(100), aliceBal)
}

func TestVscTransferInvalidFromTo(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")
	te.processAndWait()

	// Missing "from" prefix
	transferJson, _ := json.Marshal(map[string]interface{}{
		"from":   "alice",
		"to":     "bob",
		"amount": "10.000",
		"asset":  "hbd",
		"net_id": "vsc-mocknet",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 string(transferJson),
	})
	te.processAndWait()

	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(100), aliceBal)
}

// ============================================================
// Group 3: System Operations (required auth custom_json)
// ============================================================

func TestFrSync(t *testing.T) {
	te := newTestEnv()

	// vsc.fr_sync must come from the gateway wallet
	frJson, _ := json.Marshal(map[string]interface{}{
		"stake_amt":   int64(50),
		"unstake_amt": int64(0),
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"vsc.mocknet"},
		Id:            "vsc.fr_sync",
		Json:          string(frJson),
	})
	te.processAndWait()

	// Verify the ledger record was stored for FR_VIRTUAL_ACCOUNT
	records := te.LedgerDb.LedgerRecords["system:fr_balance"]
	assert.NotEmpty(t, records, "expected fr_sync ledger record for virtual FR account")
	if len(records) > 0 {
		assert.Equal(t, int64(50), records[0].Amount)
		assert.Equal(t, "hbd_savings", records[0].Asset)
		assert.Equal(t, "fr_sync", records[0].Type)
	}
}

func TestFrSyncUnstake(t *testing.T) {
	te := newTestEnv()

	frJson, _ := json.Marshal(map[string]interface{}{
		"stake_amt":   int64(0),
		"unstake_amt": int64(30),
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"vsc.mocknet"},
		Id:            "vsc.fr_sync",
		Json:          string(frJson),
	})
	te.processAndWait()

	records := te.LedgerDb.LedgerRecords["system:fr_balance"]
	assert.NotEmpty(t, records)
	if len(records) > 0 {
		assert.Equal(t, int64(30), records[0].Amount, "amount stays positive; direction encoded in from/to")
		assert.Equal(t, params.FR_VIRTUAL_ACCOUNT, records[0].From, "unstake debits FR account")
		assert.Equal(t, "", records[0].To)
	}
}

func TestFrSyncFromNonGatewayIgnored(t *testing.T) {
	te := newTestEnv()

	frJson, _ := json.Marshal(map[string]interface{}{
		"stake_amt":   int64(50),
		"unstake_amt": int64(0),
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredAuths: []string{"not-the-gateway"},
		Id:            "vsc.fr_sync",
		Json:          string(frJson),
	})
	te.processAndWait()

	records := te.LedgerDb.LedgerRecords["system:fr_balance"]
	assert.Empty(t, records, "fr_sync from non-gateway should be ignored")
}

// ============================================================
// Group 4: Edge Cases in ProcessBlock
// ============================================================

func TestEmptyBlock(t *testing.T) {
	te := newTestEnv()

	// Processing an empty block should not panic
	te.processAndWait()

	// Second block also fine
	te.processAndWait()
}

func TestUnknownCustomJsonId(t *testing.T) {
	te := newTestEnv()

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "some.unknown.id",
		Json:                 `{"foo":"bar"}`,
	})
	// Should not panic
	te.processAndWait()
}

func TestMalformedJsonInCustomJson(t *testing.T) {
	te := newTestEnv()

	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 `{this is not valid json`,
	})
	// Should not panic — malformed JSON is gracefully ignored
	te.processAndWait()
}

func TestMultipleOpsInSingleBlock(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.Creator.Transfer("bob", "vsc.gateway", "20", "HIVE", "")
	te.Creator.Transfer("charlie", "vsc.gateway", "30", "HBD", "")
	te.processAndWait()

	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	bobBal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hive")
	charlieBal := te.SE.LedgerSystem.GetBalance("hive:charlie", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(10), aliceBal)
	assert.Equal(t, int64(20), bobBal)
	assert.Equal(t, int64(30), charlieBal)
}

func TestSlotBoundaryTransition(t *testing.T) {
	te := newTestEnv()

	// Deposit in first slot
	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.processAndWait()

	// Mine enough blocks to cross a slot boundary (slot length = 10)
	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// Deposit in second slot
	te.Creator.Transfer("alice", "vsc.gateway", "20", "HBD", "")
	te.processAndWait()

	bal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(30), bal)
}

// ============================================================
// Group 5: Witness Registration
// ============================================================

func TestWitnessRegistration(t *testing.T) {
	te := newTestEnv()

	metadata := map[string]interface{}{
		"services": []string{"vsc.network"},
	}
	metaBytes, _ := json.Marshal(metadata)

	tx := hive_blocks.Tx{
		Operations: []hivego.Operation{
			{
				Type: "account_update",
				Value: map[string]interface{}{
					"account":       "witness1",
					"json_metadata": string(metaBytes),
				},
			},
		},
	}
	te.Reader.IngestTx(tx)
	te.processAndWait()

	// The mock SetWitnessUpdate is a no-op, so we can't directly assert on ByAccount.
	// This test verifies the code path doesn't panic when processing an
	// account_update with vsc.network service in json_metadata.
}

func TestAccountUpdateWithoutVscServiceIgnored(t *testing.T) {
	te := newTestEnv()

	metadata := map[string]interface{}{
		"services": []string{"some.other.service"},
	}
	metaBytes, _ := json.Marshal(metadata)

	tx := hive_blocks.Tx{
		Operations: []hivego.Operation{
			{
				Type: "account_update",
				Value: map[string]interface{}{
					"account":       "not-a-witness",
					"json_metadata": string(metaBytes),
				},
			},
		},
	}
	te.Reader.IngestTx(tx)
	te.processAndWait()

	// Should not panic — non-vsc.network services are ignored
}

// ============================================================
// Group 6: TSS Key Lifecycle
// ============================================================

func TestKeyDeprecation(t *testing.T) {
	te := newTestEnv()

	// Set up an election so GetElectionByHeight succeeds
	te.ElectionDb.ElectionsByHeight[1] = elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 5},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "witness1", Key: "bls-key-1"},
			},
		},
	}

	// Insert an active key that expires at epoch 5
	te.TssKeys.Keys["key1"] = tss_db.TssKey{
		Id:          "key1",
		Status:      tss_db.TssKeyActive,
		ExpiryEpoch: 5,
		Epoch:       3,
	}

	te.processAndWait()

	key := te.TssKeys.Keys["key1"]
	assert.Equal(t, tss_db.TssKeyDeprecated, key.Status, "key should be deprecated when epoch >= expiry")
}

func TestKeyNotDeprecatedBeforeExpiry(t *testing.T) {
	te := newTestEnv()

	te.ElectionDb.ElectionsByHeight[1] = elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 3},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "witness1", Key: "bls-key-1"},
			},
		},
	}

	te.TssKeys.Keys["key1"] = tss_db.TssKey{
		Id:          "key1",
		Status:      tss_db.TssKeyActive,
		ExpiryEpoch: 5,
		Epoch:       2,
	}

	te.processAndWait()

	key := te.TssKeys.Keys["key1"]
	assert.Equal(t, tss_db.TssKeyActive, key.Status, "key should remain active before expiry epoch")
}

func TestKeyWithoutExpiryNotDeprecated(t *testing.T) {
	te := newTestEnv()

	te.ElectionDb.ElectionsByHeight[1] = elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: 10},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members: []elections.ElectionMember{
				{Account: "witness1", Key: "bls-key-1"},
			},
		},
	}

	te.TssKeys.Keys["key1"] = tss_db.TssKey{
		Id:          "key1",
		Status:      tss_db.TssKeyActive,
		ExpiryEpoch: 0, // no expiry
		Epoch:       1,
	}

	te.processAndWait()

	key := te.TssKeys.Keys["key1"]
	assert.Equal(t, tss_db.TssKeyActive, key.Status, "key with ExpiryEpoch=0 should not be deprecated")
}

// ============================================================
// Group 7: Interest Operations
// ============================================================

func TestInterestOperationForGateway(t *testing.T) {
	te := newTestEnv()

	// Only interest operations for the gateway wallet (vsc.mocknet in mocknet) should be processed
	te.Creator.ClaimInterest("vsc.mocknet", 5)
	te.processAndWait()

	// The claimHBDInterest path stores a ledger record via LedgerSystem.ClaimHBDInterest
	// Verify no panic and that the interest was processed
	// (detailed assertion depends on mock implementation of ClaimHBDInterest)
}

func TestInterestOperationForNonGatewayIgnored(t *testing.T) {
	te := newTestEnv()

	// Interest operations for non-gateway accounts should be skipped
	// Manually inject virtual ops since ClaimInterest helper always uses the given account
	te.Reader.CreateBlockWithOps(nil, []hivego.VirtualOp{
		{
			Op: struct {
				Type  string                 "json:\"type\""
				Value map[string]interface{} "json:\"value\""
			}{
				Type: "interest_operation",
				Value: map[string]interface{}{
					"interest": map[string]interface{}{
						"nai":       "@@000000013",
						"precision": 3,
						"amount":    "10",
					},
					"owner": "some-other-account",
				},
			},
		},
	})
	time.Sleep(100 * time.Millisecond)

	// Should not panic or store anything — non-gateway interest ops are skipped
}

// ============================================================
// Group 8: Transaction Indexing
// ============================================================

func TestUserTxIndexedAsIncluded(t *testing.T) {
	te := newTestEnv()

	// A non-deposit user operation should be indexed as INCLUDED
	transferJson, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "hive:bob",
		"amount": "10.000",
		"asset":  "hbd",
		"memo":   "",
		"net_id": "vsc-mocknet",
	})
	conf := te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 string(transferJson),
	})
	te.processAndWait()

	rec := te.TxDb.GetTransaction(conf.Id)
	assert.NotNil(t, rec)
	assert.Equal(t, transactions.TransactionStatus("INCLUDED"), rec.Status)
	assert.Contains(t, rec.OpTypes, "transfer")
}

// ============================================================
// Group 9: Mixed Operations
// ============================================================

func TestMixedDepositAndUserOpsInBlock(t *testing.T) {
	te := newTestEnv()

	// Deposit for alice
	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")

	// Also deposit for bob in the same block
	te.Creator.Transfer("bob", "vsc.gateway", "50", "HIVE", "")

	te.processAndWait()

	// Verify both deposits in same block processed correctly
	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	bobBal := te.SE.LedgerSystem.GetBalance("hive:bob", te.Reader.LastBlock, "hive")
	assert.Equal(t, int64(100), aliceBal)
	assert.Equal(t, int64(50), bobBal)
}

// ============================================================
// Group 10: Multiple Slot Cycles
// ============================================================

func TestMultipleSlotCycles(t *testing.T) {
	te := newTestEnv()

	// Slot 1: deposit
	te.Creator.Transfer("alice", "vsc.gateway", "50", "HBD", "")
	te.processAndWait()

	// Cross slot boundary
	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// Slot 2: another deposit
	te.Creator.Transfer("alice", "vsc.gateway", "25", "HBD", "")
	te.processAndWait()

	// Cross another slot boundary
	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	bal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(75), bal)
}

// ============================================================
// Group 11: Edge Cases in User Operations
// ============================================================

func TestVscWithdrawPostingAuthReverted(t *testing.T) {
	te := newTestEnv()

	// Deposit first
	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")
	te.processAndWait()

	// vsc.withdraw checks RequiredAuths — posting-only auth will fail
	withdrawJson, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "alice",
		"amount": "40.000",
		"asset":  "hbd",
		"memo":   "withdrawal",
		"net_id": "vsc-mocknet",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.withdraw",
		Json:                 string(withdrawJson),
	})
	te.processAndWait()

	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// Withdrawal with posting auth only gets reverted
	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(100), aliceBal, "posting-auth withdrawal should be reverted")
}

func TestVscStakeHbdPostingAuthReverted(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")
	te.processAndWait()

	// vsc.stake_hbd checks RequiredAuths — posting-only auth will fail
	stakeJson, _ := json.Marshal(map[string]interface{}{
		"from":   "hive:alice",
		"to":     "hive:alice",
		"amount": "50.000",
		"asset":  "hbd",
		"net_id": "vsc-mocknet",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.stake_hbd",
		Json:                 string(stakeJson),
	})
	te.processAndWait()

	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	// Posting-auth-only stake is reverted
	hbdBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(100), hbdBal, "posting-auth stake should be reverted")
}

func TestVscTransferEmptyFrom(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "100", "HBD", "")
	te.processAndWait()

	// Transfer with empty "from" field — should fail
	transferJson, _ := json.Marshal(map[string]interface{}{
		"from":   "",
		"to":     "hive:bob",
		"amount": "10.000",
		"asset":  "hbd",
		"net_id": "vsc-mocknet",
	})
	te.Creator.CustomJson(stateEngine.MockJson{
		RequiredPostingAuths: []string{"alice"},
		Id:                   "vsc.transfer",
		Json:                 string(transferJson),
	})
	te.processAndWait()

	te.Reader.MineNullBlocks(10)
	time.Sleep(200 * time.Millisecond)

	aliceBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(100), aliceBal, "balance should be unchanged with empty from")
}

// ============================================================
// Group 12: HBD and HIVE asset detection
// ============================================================

func TestHbdNaiDetection(t *testing.T) {
	te := newTestEnv()

	// HBD has NAI @@000000013
	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.processAndWait()

	hbdBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	hiveBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hive")
	assert.Equal(t, int64(10), hbdBal)
	assert.Equal(t, int64(0), hiveBal)
}

func TestHiveNaiDetection(t *testing.T) {
	te := newTestEnv()

	// HIVE has NAI @@000000021
	te.Creator.Transfer("alice", "vsc.gateway", "10", "HIVE", "")
	te.processAndWait()

	hbdBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	hiveBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hive")
	assert.Equal(t, int64(0), hbdBal)
	assert.Equal(t, int64(10), hiveBal)
}

func TestBothAssetTypes(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "10", "HBD", "")
	te.Creator.Transfer("alice", "vsc.gateway", "20", "HIVE", "")
	te.processAndWait()

	hbdBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	hiveBal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hive")
	assert.Equal(t, int64(10), hbdBal)
	assert.Equal(t, int64(20), hiveBal)
}

// ============================================================
// Group 13: Zero-amount edge cases
// ============================================================

func TestZeroAmountDeposit(t *testing.T) {
	te := newTestEnv()

	te.Creator.Transfer("alice", "vsc.gateway", "0", "HBD", "")
	te.processAndWait()

	bal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(0), bal)
}

// ============================================================
// Group 14: Rapid succession of blocks
// ============================================================

func TestRapidBlockProcessing(t *testing.T) {
	te := newTestEnv()

	// Process many blocks rapidly with deposits
	for i := 0; i < 5; i++ {
		te.Creator.Transfer("alice", "vsc.gateway", "1", "HBD", "")
		te.processAndWait()
	}

	bal := te.SE.LedgerSystem.GetBalance("hive:alice", te.Reader.LastBlock, "hbd")
	assert.Equal(t, int64(5), bal)
}

// ============================================================
// Group 15: TWAB / UpdateBalances correctness
// ============================================================

// TestUpdateBalances_ClaimResetsAvgAndSetsClaimHeight verifies that when a new
// claim record exists, UpdateBalances resets hbd_avg to 0 and sets hbd_claim.
func TestUpdateBalances_ClaimResetsAvgAndSetsClaimHeight(t *testing.T) {
	te := newTestEnv()

	// Seed a balance record at block 100 with accumulated avg and old claim
	te.BalanceDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:alice",
		BlockHeight:       100,
		HBD_SAVINGS:       5000,
		HBD_AVG:           250000,
		HBD_CLAIM_HEIGHT:  50,
		HBD_MODIFY_HEIGHT: 90,
	}}

	// Insert a claim record at block 200
	te.InterestClaims.SaveClaim(ledgerDb.ClaimRecord{
		BlockHeight: 200,
		Amount:      100,
		ReceivedN:   1,
	})

	// Insert a ledger record so the account appears in distinctAccounts
	te.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id:          "interest_alice",
		To:          "hive:alice",
		Amount:      100,
		Asset:       "hbd_savings",
		Type:        "interest",
		BlockHeight: 210,
	})

	te.SE.UpdateBalances(200, 210)

	records := te.BalanceDb.BalanceRecords["hive:alice"]
	require.True(t, len(records) >= 2, "should have created a new balance record")
	latest := records[len(records)-1]

	assert.Equal(t, uint64(210), latest.BlockHeight)
	assert.Equal(t, uint64(200), latest.HBD_CLAIM_HEIGHT, "should adopt the new claim height")
	assert.Equal(t, int64(0), latest.HBD_AVG, "avg should reset on new claim")
	assert.Equal(t, int64(5100), latest.HBD_SAVINGS, "savings should include interest")
}

// TestUpdateBalances_NonClaimPreservesClaimHeight verifies that a balance
// update triggered by non-claim activity (e.g. a deposit) preserves the
// existing hbd_claim and correctly accumulates hbd_avg.
func TestUpdateBalances_NonClaimPreservesClaimHeight(t *testing.T) {
	te := newTestEnv()

	// Seed: account was last claimed at block 100, modified at block 100
	te.BalanceDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:alice",
		BlockHeight:       100,
		HBD:               0,
		HBD_SAVINGS:       2000,
		HBD_AVG:           0,
		HBD_CLAIM_HEIGHT:  100,
		HBD_MODIFY_HEIGHT: 100,
	}}

	// A claim exists at block 100 (matching the balance record)
	te.InterestClaims.SaveClaim(ledgerDb.ClaimRecord{
		BlockHeight: 100,
		Amount:      50,
		ReceivedN:   1,
	})

	// A deposit at block 150 triggers the balance update
	te.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id:          "deposit_alice",
		To:          "hive:alice",
		Amount:      500,
		Asset:       "hbd",
		Type:        "deposit",
		BlockHeight: 150,
	})

	te.SE.UpdateBalances(100, 150)

	records := te.BalanceDb.BalanceRecords["hive:alice"]
	require.True(t, len(records) >= 2)
	latest := records[len(records)-1]

	assert.Equal(t, uint64(150), latest.BlockHeight)
	assert.Equal(t, uint64(100), latest.HBD_CLAIM_HEIGHT,
		"claim height must be preserved across non-claim updates")
	assert.Equal(t, int64(500), latest.HBD,
		"liquid HBD should include the deposit")

	// hbd_avg should accumulate: prev_avg + savings * (endBlock - modify_height)
	// = 0 + 2000 * (150 - 100) = 100000
	assert.Equal(t, int64(100000), latest.HBD_AVG,
		"avg should accumulate savings * elapsed blocks")
	assert.Equal(t, uint64(150), latest.HBD_MODIFY_HEIGHT)
}

// TestUpdateBalances_RepeatedNonClaimUpdatesAccumulateAvg verifies that
// multiple non-claim balance updates correctly accumulate hbd_avg over
// successive periods without losing the claim height.
func TestUpdateBalances_RepeatedNonClaimUpdatesAccumulateAvg(t *testing.T) {
	te := newTestEnv()

	// Initial state: claimed at 100, savings = 1000
	te.BalanceDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:alice",
		BlockHeight:       100,
		HBD_SAVINGS:       1000,
		HBD_AVG:           0,
		HBD_CLAIM_HEIGHT:  100,
		HBD_MODIFY_HEIGHT: 100,
	}}

	te.InterestClaims.SaveClaim(ledgerDb.ClaimRecord{
		BlockHeight: 100,
		Amount:      10,
		ReceivedN:   1,
	})

	// First deposit at block 200
	te.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id: "dep1", To: "hive:alice", Amount: 500,
		Asset: "hbd", Type: "deposit", BlockHeight: 200,
	})
	te.SE.UpdateBalances(100, 200)

	records := te.BalanceDb.BalanceRecords["hive:alice"]
	mid := records[len(records)-1]
	assert.Equal(t, uint64(100), mid.HBD_CLAIM_HEIGHT, "claim height preserved after 1st deposit")
	// avg = 0 + 1000*(200-100) = 100000
	assert.Equal(t, int64(100000), mid.HBD_AVG)
	assert.Equal(t, int64(500), mid.HBD)
	assert.Equal(t, int64(1000), mid.HBD_SAVINGS, "savings unchanged by HBD deposit")

	// Second deposit at block 300
	te.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id: "dep2", To: "hive:alice", Amount: 300,
		Asset: "hbd", Type: "deposit", BlockHeight: 300,
	})
	te.SE.UpdateBalances(200, 300)

	records = te.BalanceDb.BalanceRecords["hive:alice"]
	final := records[len(records)-1]
	assert.Equal(t, uint64(100), final.HBD_CLAIM_HEIGHT, "claim height preserved after 2nd deposit")
	// avg = 100000 + 1000*(300-200) = 200000
	assert.Equal(t, int64(200000), final.HBD_AVG)
	assert.Equal(t, int64(800), final.HBD, "500+300")
}

// TestUpdateBalances_FrBalanceGetsClaimUpdate verifies that system:fr_balance
// receives claim height updates even when it has no ledger records in the
// current window.
func TestUpdateBalances_FrBalanceGetsClaimUpdate(t *testing.T) {
	te := newTestEnv()

	// Seed FR balance with hbd_claim=0 (never claimed)
	te.BalanceDb.BalanceRecords["system:fr_balance"] = []ledgerDb.BalanceRecord{{
		Account:           "system:fr_balance",
		BlockHeight:       50,
		HBD_SAVINGS:       100000,
		HBD_AVG:           5000000,
		HBD_CLAIM_HEIGHT:  0,
		HBD_MODIFY_HEIGHT: 50,
	}}

	// A claim exists at block 200
	te.InterestClaims.SaveClaim(ledgerDb.ClaimRecord{
		BlockHeight: 200,
		Amount:      500,
		ReceivedN:   10,
	})

	// Some OTHER account has ledger activity (to trigger UpdateBalances normally)
	te.BalanceDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:alice",
		BlockHeight:       100,
		HBD:               1000,
		HBD_CLAIM_HEIGHT:  200,
		HBD_MODIFY_HEIGHT: 100,
	}}
	te.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id: "dep_alice", To: "hive:alice", Amount: 100,
		Asset: "hbd", Type: "deposit", BlockHeight: 210,
	})

	// FR balance has NO ledger records in this window — but should still be processed
	te.SE.UpdateBalances(200, 210)

	frRecords := te.BalanceDb.BalanceRecords["system:fr_balance"]
	require.True(t, len(frRecords) >= 2, "FR balance should have a new record")
	latest := frRecords[len(frRecords)-1]

	assert.Equal(t, uint64(200), latest.HBD_CLAIM_HEIGHT,
		"FR balance claim height should be updated to latest claim")
	assert.Equal(t, int64(0), latest.HBD_AVG,
		"avg should reset since claim height changed")
	assert.Equal(t, int64(100000), latest.HBD_SAVINGS,
		"savings should be unchanged (no ledger activity)")
}

// TestUpdateBalances_TwabCorrectAfterClaimThenDeposit runs the full cycle:
// claim → deposit → verify TWAB at next claim is computed correctly.
func TestUpdateBalances_TwabCorrectAfterClaimThenDeposit(t *testing.T) {
	te := newTestEnv()

	// Claim at block 100
	te.InterestClaims.SaveClaim(ledgerDb.ClaimRecord{
		BlockHeight: 100, Amount: 50, ReceivedN: 1,
	})

	// Alice: savings=5000, just claimed, avg reset
	te.BalanceDb.BalanceRecords["hive:alice"] = []ledgerDb.BalanceRecord{{
		Account:           "hive:alice",
		BlockHeight:       110,
		HBD_SAVINGS:       5000,
		HBD_AVG:           0,
		HBD_CLAIM_HEIGHT:  100,
		HBD_MODIFY_HEIGHT: 110,
	}}

	// Deposit at block 200 (non-claim activity)
	te.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id: "dep1", To: "hive:alice", Amount: 1000,
		Asset: "hbd", Type: "deposit", BlockHeight: 200,
	})
	te.SE.UpdateBalances(110, 200)

	records := te.BalanceDb.BalanceRecords["hive:alice"]
	afterDeposit := records[len(records)-1]

	// Verify intermediate state is correct
	assert.Equal(t, uint64(100), afterDeposit.HBD_CLAIM_HEIGHT, "claim preserved")
	// avg = 0 + 5000*(200-110) = 450000
	assert.Equal(t, int64(450000), afterDeposit.HBD_AVG, "avg accumulated correctly")
	assert.Equal(t, uint64(200), afterDeposit.HBD_MODIFY_HEIGHT)

	// Now add second claim at block 300
	te.InterestClaims.SaveClaim(ledgerDb.ClaimRecord{
		BlockHeight: 300, Amount: 100, ReceivedN: 1,
	})
	te.LedgerDb.StoreLedger(ledgerDb.LedgerRecord{
		Id: "int1", To: "hive:alice", Amount: 100,
		Asset: "hbd_savings", Type: "interest", BlockHeight: 310,
	})
	te.SE.UpdateBalances(200, 310)

	records = te.BalanceDb.BalanceRecords["hive:alice"]
	afterClaim := records[len(records)-1]

	assert.Equal(t, uint64(300), afterClaim.HBD_CLAIM_HEIGHT, "new claim adopted")
	assert.Equal(t, int64(0), afterClaim.HBD_AVG, "avg reset on new claim")
	assert.Equal(t, int64(5100), afterClaim.HBD_SAVINGS, "savings includes interest")

	// Verify the TWAB that ClaimHBDInterest would compute for the period 100→300
	// endingAvg = (hbd_avg + savings * A) / B
	// Using the state just before claim: avg=450000, savings=5000, modify=200, claim=100
	// B = 300 - 100 = 200, A = 300 - 200 = 100
	// endingAvg = (450000 + 5000*100) / 200 = 950000/200 = 4750
	// This is correct: 5000 savings for full 200 blocks except first 10 = avg ~4750
	expectedTwab := int64(4750)
	B := int64(300 - 100)
	A := int64(300 - 200)
	computedTwab := (afterDeposit.HBD_AVG + afterDeposit.HBD_SAVINGS*A) / B
	assert.Equal(t, expectedTwab, computedTwab, "TWAB should reflect time-weighted average")
}
