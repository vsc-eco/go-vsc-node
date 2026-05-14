package main

import (
	"strings"
	"vsc-node/modules/hive/streamer"

	"github.com/vsc-eco/hivego"
)

var MAINNET_CLAIM_START = uint64(95900000)
var filter = func(op hivego.Operation, blockParams *streamer.BlockParams) bool {
	if op.Type == "custom_json" {
		if strings.HasPrefix(op.Value["id"].(string), "vsc.") {
			return true
		}
	}
	if op.Type == "account_update" || op.Type == "account_update2" {
		return true
	}

	// feed_publish + witness_set_properties feed the incentive pendulum's
	// sole-HIVE oracle (FeedTracker): feed_publish carries the HIVE/HBD price,
	// witness_set_properties carries hbd_interest_rate. The FeedTracker gates
	// these to active witnesses internally, so no per-account check here.
	if op.Type == "feed_publish" || op.Type == "witness_set_properties" {
		return true
	}

	if op.Type == "transfer" || op.Type == "transfer_to_savings" || op.Type == "transfer_from_savings" {
		if strings.HasPrefix(op.Value["to"].(string), "vsc.") {
			if blockParams.BlockHeight > MAINNET_CLAIM_START {
				if op.Type == "transfer_to_savings" || op.Type == "transfer_from_savings" {
					blockParams.NeedsVirtualOps = true
				}
			}
			return true
		}

		if strings.HasPrefix(op.Value["from"].(string), "vsc.") {
			if blockParams.BlockHeight > MAINNET_CLAIM_START {
				if op.Type == "transfer_to_savings" || op.Type == "transfer_from_savings" {
					blockParams.NeedsVirtualOps = true
				}
			}
			return true
		}
	}

	return false
}
