package main

import (
	"fmt"
	"os"

	p2pInterface "vsc-node/lib/p2p"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/hive_blocks"
	"vsc-node/modules/db/vsc/witnesses"
	"vsc-node/modules/hive/streamer"

	"github.com/vsc-eco/hivego"
)

func main() {
	db := db.New()
	vscDb := vsc.New(db)
	witnesses := witnesses.New(vscDb)
	hiveBlocks := hive_blocks.New(vscDb)

	// choose the source
	blockClient := hivego.NewHiveRpc("https://api.hive.blog")
	filter := func(op hivego.Operation) bool { return true }
	filters := []streamer.FilterFunc{filter}
	process := func(block hive_blocks.HiveBlock) error {
		fmt.Printf("processed block %d with ID %s\n", block.BlockNumber, block.BlockID)
		return nil
	}
	streamerPlugin := streamer.New(blockClient, hiveBlocks, filters, process, nil) // optional starting block #

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		db,
		vscDb,
		witnesses,
		hiveBlocks,
		streamerPlugin,
		p2pInterface.New(),
	)

	a := aggregate.New(
		plugins,
	)

	err := a.Run()
	if err != nil {
		fmt.Println("error is", err)
		os.Exit(1)
	}
}
