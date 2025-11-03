package state_engine_test

import (
	"fmt"
	"strings"
	"testing"
	"time"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"
	wasm_runtime "vsc-node/modules/wasm/runtime_ipc"

	DataLayer "vsc-node/lib/datalayer"
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

	client := hivego.NewHiveRpc("https://api.hive.blog")
	s := streamer.NewStreamer(client, hiveBlocks, []streamer.FilterFunc{filter}, []streamer.VirtualFilterFunc{
		func(op hivego.VirtualOp) bool {
			return op.Op.Type == "interest_operation"
		},
	}, nil)

	// slow down the streamer a bit for real data
	streamer.AcceptableBlockLag = 0
	streamer.BlockBatchSize = 500
	streamer.DefaultBlockStart = 81614028
	streamer.HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 250
	streamer.MinTimeBetweenBlockBatchFetches = time.Millisecond * 250
	streamer.DbPollInterval = time.Millisecond * 500

	assert.NoError(t, err)

	p2p := p2p.New(witnessesDb)
	dl := DataLayer.New(p2p, "state-engine")

	wasm := wasm_runtime.New()

	se := stateEngine.New(dl, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionDb, wasm)

	se.Commit()

	sr := streamer.NewStreamReader(hiveBlocks, se.ProcessBlock, se.SaveBlockHeight)

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		db,
		vscDb,
		electionDb,
		witnessesDb,
		contractDb,
		txDb,
		ledgerDbImpl,
		balanceDb,
		hiveBlocks,
		interestClaims,
		wasm,
		s,
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

	// slow down the streamer a bit for real data
	streamer.AcceptableBlockLag = 0
	streamer.BlockBatchSize = 500
	streamer.DefaultBlockStart = 81614028
	streamer.HeadBlockCheckPollIntervalBeforeFirstUpdate = time.Millisecond * 250
	streamer.MinTimeBetweenBlockBatchFetches = time.Millisecond * 250
	streamer.DbPollInterval = time.Millisecond * 500

	assert.NoError(t, err)

	p2p := p2p.New(witnessesDb)

	dl := DataLayer.New(p2p, "state-engine")

	wasm := wasm_runtime.New()

	se := stateEngine.New(dl, witnessesDb, electionDb, contractDb, contractState, txDb, ledgerDbImpl, balanceDb, hiveBlocks, interestClaims, vscBlocks, actionsDb, wasm)

	process := func(block hive_blocks.HiveBlock) {
		se.ProcessBlock(block)
	}

	agg := aggregate.New([]aggregate.Plugin{
		conf,
		db,
		vscDb,
		electionDb,
		witnessesDb,
		contractDb,
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

	bal := se.LedgerExecutor.Ls.GetBalance("hive:test-account", mockReader.LastBlock, "hbd")

	fmt.Println("guaranteed bal", bal)

	select {}
}
