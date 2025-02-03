package main

import (
	"fmt"
	"os"
	"strings"
	"time"

	"vsc-node/lib/hive"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/announcements"
	data_availability "vsc-node/modules/data-availability"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"
	p2pInterface "vsc-node/modules/p2p"

	wasm_parent_ipc "vsc-node/modules/wasm/parent_ipc"

	"github.com/vsc-eco/hivego"
)

func main() {
	dbConf := db.NewDbConfig()

	fmt.Println("MONGO_URL", os.Getenv("MONGO_URL"))
	err := dbConf.SetDbURI(os.Getenv("MONGO_URL"))
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	db := db.New(dbConf)
	vscDb := vsc.New(db)
	witnesses := witnesses.New(vscDb)
	hiveBlocks, err := hive_blocks.New(vscDb)
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
	announcementsConf := announcements.NewAnnouncementsConfig()

	//TODO: Pull from proper sources
	//Placeholder wif
	wif := "5Hx12hCEUxBxZRcCY41RxacDAapnsuGihx5553k6Ne1bwXLBi7s"
	kp, _ := hivego.KeyPairFromWif(wif)
	hiveCreator := hive.LiveTransactionCreator{
		TransactionCrafter: hive.TransactionCrafter{},
		TransactionBroadcaster: hive.TransactionBroadcaster{
			Client:  hiveRpcClient,
			KeyPair: kp,
		},
	}

	announcementsManager, err := announcements.New(hiveRpcClient, announcementsConf, time.Hour*24, &hiveCreator)
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	wasm := wasm_parent_ipc.New() // TODO set proper cmd path

	p2p := p2pInterface.New()

	dataAvailability := data_availability.New(p2p, announcementsConf)

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		dbConf,
		db,
		announcementsConf,
		announcementsManager,
		vscDb,
		witnesses,
		hiveBlocks,
		streamerPlugin,
		p2p,
		dataAvailability,
		wasm,
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
