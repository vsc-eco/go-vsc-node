package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"vsc-node/lib/datalayer"
	"vsc-node/lib/hive"
	"vsc-node/lib/logger"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	"vsc-node/modules/common"
	data_availability "vsc-node/modules/data-availability"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/contracts"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/hive_blocks"
	ledgerDb "vsc-node/modules/db/vsc/ledger"
	"vsc-node/modules/db/vsc/transactions"
	vscBlocks "vsc-node/modules/db/vsc/vsc_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/gql"
	"vsc-node/modules/gql/gqlgen"
	"vsc-node/modules/hive/streamer"
	p2pInterface "vsc-node/modules/p2p"
	stateEngine "vsc-node/modules/state-processing"
	transactionpool "vsc-node/modules/transaction-pool"

	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"

	"github.com/vsc-eco/hivego"
)

func main() {
	dbConf := db.NewDbConfig()

	fmt.Println("MONGO_URL", os.Getenv("MONGO_URL"))

	db := db.New(dbConf)
	vscDb := vsc.New(db)
	hiveBlocks, err := hive_blocks.New(vscDb)
	witnessDb := witnesses.New(vscDb)
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	// choose the source
	blockClient := hivego.NewHiveRpc("https://api.hive.blog")
	filter := func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
		if op.Type == "custom_json" {
			if strings.HasPrefix(op.Value["id"].(string), "vsc.") {
				return true
			}
		}
		if op.Type == "account_update" || op.Type == "account_update2" {
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
	filters := []streamer.FilterFunc{filter}
	streamerPlugin := streamer.NewStreamer(blockClient, hiveBlocks, filters, nil, nil) // optional starting block #

	// new announcements manager
	hiveRpcClient := hivego.NewHiveRpc("https://hive-api.web3telekom.xyz/")
	identityConfig := common.NewIdentityConfig()

	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: identityConfig.HiveActiveKeyPair,
		},
	}

	announcementsManager, err := announcements.New(hiveRpcClient, identityConfig, time.Hour*24, &hiveCreator)
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	wasm := wasm_parent_ipc.New() // TODO set proper cmd path

	p2p := p2pInterface.New(witnessDb)

	da := datalayer.New(p2p)

	dataAvailability := data_availability.New(p2p, identityConfig, da)

	l := logger.PrefixedLogger{}
	e := elections.New(vscDb)
	c := contracts.New(vscDb)
	cState := contracts.NewContractState(vscDb)
	tx := transactions.New(vscDb)
	le := ledgerDb.New(vscDb)
	b := ledgerDb.NewBalances(vscDb)
	intrest := ledgerDb.NewInterestClaimDb(vscDb)
	blks := vscBlocks.New(vscDb)
	actions := ledgerDb.NewActionsDb(vscDb)
	se := stateEngine.New(l, da, witnessDb, e, c, cState, tx, le, b, hiveBlocks, intrest, blks, actions, wasm)

	txPool := transactionpool.New(p2p, tx, da, identityConfig)
	gqlManager := gql.New(gqlgen.NewExecutableSchema(gqlgen.Config{Resolvers: &gqlgen.Resolver{witnessDb, txPool}}), "localhost:8080")

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		dbConf,
		db,
		identityConfig,
		announcementsManager,
		vscDb,
		witnessDb,
		hiveBlocks,
		streamerPlugin,
		p2p,
		dataAvailability,
		e, c, cState, tx, le, b, intrest, blks, actions, se,
		wasm,
		txPool,
		gqlManager,
	)

	a := aggregate.New(
		plugins,
	)

	err = a.Run()
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}
}
