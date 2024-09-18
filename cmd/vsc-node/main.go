package main

import (
	"fmt"
	"os"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	hiveStreamer "vsc-node/modules/hive/streamer"
)

func main() {
	db := db.New()
	a := aggregate.New(
		[]aggregate.Plugin{
			db,
			hiveStreamer.New(db),
		},
	)

	err := a.Run()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}
