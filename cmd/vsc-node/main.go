package main

import (
	"fmt"
	"os"
	"time"

	p2pInterface "vsc-node/lib/libp2p"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	hiveStreamer "vsc-node/modules/hive/streamer"
)

func main() {
	db := db.New()

	plugins := make([]aggregate.Plugin, 0)

	plugins = append(plugins,
		db,
		hiveStreamer.New(db),
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
	for {
		time.Sleep(2500 * time.Second)
	}
}
