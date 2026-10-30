// feed-publisher broadcasts a Hive feed_publish operation on a fixed interval so
// the pendulum oracle's FeedTracker always has a fresh price in its rolling window.
//
// On a HAF devnet, initminer is the sole Hive witness and produces all blocks,
// so its feed is trusted as soon as the 4-block minimum in the window is met.
// A feed stays fresh for the FeedTracker's FeedFreshnessBlocks (1200 blocks,
// about an hour at 3 s/block); publishing every 240 s keeps it well inside that.
//
// All configuration is read from the data-dir:
//   - config/identityConfig.json  — publisher account name + active key WIF
//   - config/hiveConfig.json      — Hive RPC URLs
//   - config/feedPublisherConfig.json — chain ID, interval, base/quote assets
//
// devnet-setup populates the first two; feedPublisherConfig.json is written
// with defaults on first startup if not already present.
//
// FEED_PUBLISHER_INTERVAL_BLOCKS, when set, paces publishing in Hive blocks
// instead of IntervalSeconds. The devnet sets it so a feed is as many blocks old
// under the harness's fast blocks as at the real 3 s cadence.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
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
	everyBlocks, err := intervalBlocksFromEnv()
	if err != nil {
		fmt.Fprintln(os.Stderr, "feed-publisher:", err)
		os.Exit(1)
	}

	if everyBlocks > 0 {
		fmt.Printf("feed-publisher: %s/%s from %s every %d blocks\n", fp.Base, fp.Quote, id.HiveUsername, everyBlocks)
	} else {
		fmt.Printf("feed-publisher: %s/%s from %s every %s\n", fp.Base, fp.Quote, id.HiveUsername, interval)
	}

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
		if everyBlocks > 0 {
			waitBlocks(func() (uint64, error) { return headBlock(client) }, everyBlocks, 250*time.Millisecond)
		} else {
			time.Sleep(interval)
		}
	}
}

// intervalBlocksFromEnv reads FEED_PUBLISHER_INTERVAL_BLOCKS (0 when unset).
func intervalBlocksFromEnv() (uint64, error) {
	v := os.Getenv("FEED_PUBLISHER_INTERVAL_BLOCKS")
	if v == "" {
		return 0, nil
	}
	n, err := strconv.ParseUint(v, 10, 64)
	if err != nil || n == 0 {
		return 0, fmt.Errorf("FEED_PUBLISHER_INTERVAL_BLOCKS=%q: want a positive block count", v)
	}
	return n, nil
}

func headBlock(client *hivego.HiveRpcNode) (uint64, error) {
	raw, err := client.GetDynamicGlobalProps()
	if err != nil {
		return 0, err
	}
	var props struct {
		HeadBlockNumber uint64 `json:"head_block_number"`
	}
	if err := json.Unmarshal(raw, &props); err != nil {
		return 0, err
	}
	return props.HeadBlockNumber, nil
}

// waitBlocks returns once the head is n blocks past where it was when called,
// retrying head reads that fail.
func waitBlocks(head func() (uint64, error), n uint64, poll time.Duration) {
	var start uint64
	for {
		h, err := head()
		if err == nil {
			start = h
			break
		}
		time.Sleep(poll)
	}
	for {
		time.Sleep(poll)
		if h, err := head(); err == nil && h >= start+n {
			return
		}
	}
}
