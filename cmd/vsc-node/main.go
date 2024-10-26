package main

import (
	"fmt"
	"os"

	p2pInterface "vsc-node/lib/p2p"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
)

func main() {
	db := db.New()
	vscDb := vsc.New(db)
	// hiveBlocks := hive_blocks.New(vscDb)

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		db,
		vscDb,
		// hiveBlocks,
		// hiveStreamer.New(hiveBlocks),
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
