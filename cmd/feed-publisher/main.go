// feed-publisher broadcasts a Hive feed_publish operation on a fixed interval so
// the pendulum oracle's FeedTracker always has a fresh price in its rolling window.
//
// On a HAF devnet, initminer is the sole Hive witness and produces all blocks,
// so its feed is trusted as soon as the 4-block minimum in the window is met.
// The feed expires after ~100 blocks (~5 min at 3 s/block); publishing every
// 240 s keeps it alive with comfortable headroom.
//
// All configuration is read from the data-dir:
//   - config/identityConfig.json  — publisher account name + active key WIF
//   - config/hiveConfig.json      — Hive RPC URLs
//   - config/feedPublisherConfig.json — chain ID, interval, base/quote assets
//
// devnet-setup populates the first two; feedPublisherConfig.json is written
// with defaults on first startup if not already present.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"

	"vsc-node/modules/common"
	"vsc-node/modules/hive/streamer"

	"github.com/vsc-eco/hivego"
)

func main() {
	dataDir := flag.String("data-dir", "data", "Data directory containing the config subdirectory")
	initFlag := flag.Bool("init", false, "Write default config files to data-dir and exit")
	flag.Parse()

	idConf := common.NewIdentityConfig(*dataDir)
	if err := idConf.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "feed-publisher: identity config:", err)
		os.Exit(1)
	}

	hiveConf := streamer.NewHiveConfig(*dataDir)
	if err := hiveConf.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "feed-publisher: hive config:", err)
		os.Exit(1)
	}

	fpConf := newFeedPublisherConfig(*dataDir)
	if err := fpConf.Init(); err != nil {
		fmt.Fprintln(os.Stderr, "feed-publisher: feed config:", err)
		os.Exit(1)
	}

	if *initFlag {
		fmt.Println("feed-publisher: config written to", *dataDir)
		return
	}

	id := idConf.Get()
	fp := fpConf.Get()
	uris := hiveConf.GetHiveURIs()

	op := feedPublishOperation{Publisher: id.HiveUsername}
	op.ExchangeRate.Base = fp.Base
	op.ExchangeRate.Quote = fp.Quote
	interval := time.Duration(fp.IntervalSeconds) * time.Second

	fmt.Printf("feed-publisher: %s/%s from %s every %s\n", fp.Base, fp.Quote, id.HiveUsername, interval)

	for {
		client := hivego.NewHiveRpc(uris)
		client.ChainID = fp.ChainID
		wif := id.HiveActiveKey
		txID, err := client.Broadcast([]hivego.HiveOperation{op}, &wif)
		if err != nil {
			fmt.Fprintf(os.Stderr, "feed-publisher: broadcast failed: %v\n", err)
		} else {
			fmt.Printf("feed-publisher: tx=%s\n", txID)
		}
		time.Sleep(interval)
	}
}
