package state_engine_test

import (
	"testing"
	"time"
	"vsc-node/lib/test_utils"
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
)

func TestMockEngine(t *testing.T) {
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

	mockCreator.Transfer("test-account", "vsc.gateway", "10", "HBD", "test transfer")
	mockReader.CreateBlock()

	// witnessBlock() runs ProcessFunction in a goroutine
	time.Sleep(100 * time.Millisecond)

	bal := se.LedgerSystem.GetBalance("hive:test-account", mockReader.LastBlock, "hbd")
	assert.Equal(t, int64(10), bal)
}
