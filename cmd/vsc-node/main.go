package main

import (
	"fmt"
	"os"

	p2pInterface "vsc-node/lib/p2p"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"
	hiveStreamer "vsc-node/modules/hive/streamer"
)

func main() {
	db := db.New()
	vscDb := vsc.New(db)
	witnesses := witnesses.New(vscDb)

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		db,
		vscDb,
		witnesses,
		hiveStreamer.New(witnesses),
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
