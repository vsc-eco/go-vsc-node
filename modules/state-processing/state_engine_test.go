package state_engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/common"
	"vsc-node/modules/common/common_types"
	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/nonces"
	rcDb "vsc-node/modules/db/vsc/rcs"
	"vsc-node/modules/db/vsc/transactions"
	tss_db "vsc-node/modules/db/vsc/tss"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	blockconsumer "vsc-node/modules/hive/block-consumer"
	"vsc-node/modules/hive/streamer"
	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"

	DataLayer "vsc-node/lib/datalayer"
	"vsc-node/lib/logger"
	"vsc-node/lib/test_utils"
	p2p "vsc-node/modules/p2p"

	stateEngine "vsc-node/modules/state-processing"

	"github.com/stretchr/testify/assert"
	"github.com/vsc-eco/hivego"
)

func TestStateEngine(t *testing.T) {
	conf := db.NewDbConfig()
	db := db.New(conf)
	vscDb := vsc.New(db)
	hiveBlocks, err := hive_blocks.New(vscDb)
	electionDb := elections.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)
	vscBlocks := vscBlocks.New(vscDb)
	actionDb := ledgerDb.NewActionsDb(vscDb)
	rcDb := rcDb.New(vscDb)
	nonceDb := nonces.New(vscDb)
	tssKeys := tss_db.NewKeys(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)
	identityConfig := common.NewIdentityConfig()
	sysConfig := systemconfig.MocknetConfig()
	l := logger.PrefixedLogger{
		Prefix: "vsc-node",
	}

	filter := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		if op.Type == "custom_json" {
			if strings.HasPrefix(op.Value["id"].(string), "vsc.") {
				return true
			}
		}
		if op.Type == "account_update" || op.Type == "account_update2" {
			return true
		}

		if op.Type == "transfer_from_savings" {
			if strings.HasPrefix(op.Value["to"].(string), "vsc.") {
				blockParams.NeedsVirtualOps = true
			}
			return true
		}

		if op.Type == "transfer" || op.Type == "transfer_to_savings" {
			if strings.HasPrefix(op.Value["to"].(string), "vsc.") {
				return true
			}

			if strings.HasPrefix(op.Value["from"].(string), "vsc.") {
				return true
			}
		}

		return false
	}

	client := hivego.NewHiveRpc("https://techcoderx.com")
	s := streamer.NewStreamer(client, hiveBlocks, []streamer.FilterFunc{filter}, []streamer.VirtualFilterFunc{
		func(op hivego.VirtualOp) bool {
			return op.Op.Type == "interest_operation"
		},
	}, nil)

	// slow down the streamer a bit for real data
	streamer.AcceptableBlockLag = 0
	streamer.BlockBatchSize = 500
	streamer.DefaultBlockStart = 94601000
	streamer.HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 250
	streamer.MinTimeBetweenBlockBatchFetches = time.Millisecond * 250
	streamer.DbPollInterval = time.Millisecond * 500

	assert.NoError(t, err)

	var blockStatus common_types.BlockStatusGetter = nil
	p2p := p2p.New(witnessesDb, identityConfig, sysConfig, blockStatus)
	dl := DataLayer.New(p2p, "state-engine")

	wasm := wasm_runtime.New()

	se := stateEngine.New(l, sysConfig, dl, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionDb, rcDb, nonceDb, tssKeys, tssCommitments, tssRequests, wasm)

	blockConsumer := blockconsumer.New(se)
	sr := streamer.NewStreamReader(hiveBlocks, blockConsumer.ProcessBlock, se.SaveBlockHeight, streamer.DefaultBlockStart)

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		db,
		vscDb,
		electionDb,
		witnessesDb,
		contractDb,
		contractState,
		txDb,
		ledgerDbImpl,
		balanceDb,
		hiveBlocks,
		interestClaims,
		wasm,
		blockConsumer,
		s,
		se,
		sr,
	})

	test_utils.RunPlugin(t, agg)

	fmt.Println(err)

	select {}
}

func TestMockEngine(t *testing.T) {
	conf := db.NewDbConfig()
	db := db.New(conf)
	vscDb := vsc.New(db)
	hiveBlocks, err := hive_blocks.New(vscDb)
	electionDb := elections.New(vscDb)
	witnessesDb := witnesses.New(vscDb)
	contractDb := contracts.New(vscDb)
	txDb := transactions.New(vscDb)
	ledgerDbImpl := ledgerDb.New(vscDb)
	balanceDb := ledgerDb.NewBalances(vscDb)
	interestClaims := ledgerDb.NewInterestClaimDb(vscDb)
	contractState := contracts.NewContractState(vscDb)
	vscBlocks := vscBlocks.New(vscDb)
	actionsDb := ledgerDb.NewActionsDb(vscDb)
	rcDb := rcDb.New(vscDb)
	nonceDb := nonces.New(vscDb)
	tssKeys := tss_db.NewKeys(vscDb)
	tssCommitments := tss_db.NewCommitments(vscDb)
	tssRequests := tss_db.NewRequests(vscDb)
	identityConfig := common.NewIdentityConfig()
	sysConfig := systemconfig.MocknetConfig()
	l := logger.PrefixedLogger{
		Prefix: "vsc-node",
	}

	// slow down the streamer a bit for real data
	streamer.AcceptableBlockLag = 0
	streamer.BlockBatchSize = 500
	streamer.DefaultBlockStart = 81614028
	streamer.HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 250
	streamer.MinTimeBetweenBlockBatchFetches = time.Millisecond * 250
	streamer.DbPollInterval = time.Millisecond * 500

	assert.NoError(t, err)

	p2p := p2p.New(witnessesDb, identityConfig, sysConfig, nil)

	dl := DataLayer.New(p2p, "state-engine")

	wasm := wasm_runtime.New()

	se := stateEngine.New(l, sysConfig, dl, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb, rcDb, nonceDb, tssKeys, tssCommitments, tssRequests, wasm)

	process := func(block hive_blocks.HiveBlock, headHeight *uint64) {
		se.ProcessBlock(block)
	}

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		db,
		vscDb,
		electionDb,
		witnessesDb,
		contractDb,
		contractState,
		txDb,
		ledgerDbImpl,
		balanceDb,
		hiveBlocks,
		interestClaims,
		wasm,
	})

	// go func() {
	test_utils.RunPlugin(t, agg)
	// }()

	mockReader := &stateEngine.MockReader{
		ProcessFunction: process,
	}

	mockReader.StartRealtime()

	mockCreator := stateEngine.MockCreator{
		Mr: mockReader,
	}

	// mockCreator.CustomJson(stateEngine.MockJson{
	// 	Id:                   "vsc.test",
	// 	Json:                 `{"action": ":3"}`,
	// 	RequiredAuths:        []string{"test-account"},
	// 	RequiredPostingAuths: []string{},
	// })

	mockCreator.Transfer("test-account", "vsc.gateway", "10", "HBD", "test transfer")

	// mockCreator.ClaimInterest("test-account", 1000)

	time.Sleep(10 * time.Second)

	bal := se.LedgerSystem.GetBalance("hive:test-account", mockReader.LastBlock, "hbd")

	fmt.Println("guaranteed bal", bal)

	select {}
}
