package main

import (
	"fmt"
	"os"
	"strings"

	p2pInterface "vsc-node/lib/p2p"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"

	"vsc-node/modules/wasm/parent_ipc"

	"github.com/vsc-eco/hivego"
)

func main() {
	conf := db.NewDbConfig()
	db := db.New(conf)
	vscDb := vsc.New(db)
	witnesses := witnesses.New(vscDb)
	hiveBlocks, err := hive_blocks.New(vscDb)
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}

	// choose the source
	blockClient := hivego.NewHiveRpc("https://api.hive.blog")
	filter := func(op hivego.Operation) bool {
		if op.Type == "custom_json" {
			if strings.HasPrefix(op.Value["id"].(string), "vsc.") {
				return true
			}
		}
		if op.Type == "account_update" || op.Type == "account_update2" {
			return true
		}

		if op.Type == "transfer" {
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
	streamerPlugin := streamer.NewStreamer(blockClient, hiveBlocks, filters, nil) // optional starting block #

	wasm := wasm_parent_ipc.New() // TODO set proper cmd path

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		conf,
		db,
		vscDb,
		witnesses,
		hiveBlocks,
		streamerPlugin,
		p2pInterface.New(),
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
