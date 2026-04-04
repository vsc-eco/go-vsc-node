package devnet

import (
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/vsc-eco/hivego"
)

// fundAccounts publishes a witness price feed so TBD (test HBD)
// can be created, waits for accumulation, then transfers TBD to
// the first witness for contract deployment fees and RC credits.
func (d *Devnet) fundAccounts() error {
	hiveClient := hivego.NewHiveRpc([]string{d.HiveRPCEndpoint()})
	hiveClient.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e" // testnet
	wif := d.cfg.InitminerWIF

	// Step 1: publish a price feed so block rewards produce TBD.
	// Use a raw CustomJson since hivego doesn't have a FeedPublish struct.
	log.Printf("[devnet] publishing witness price feed...")
	feedOp := feedPublishOp("initminer", "1.000 TBD", "1.000 TESTS")
	_, err := hiveClient.Broadcast([]hivego.HiveOperation{feedOp}, &wif)
	if err != nil {
		return fmt.Errorf("publishing price feed: %w", err)
	}

	// Step 2: wait for TBD to accumulate via block rewards.
	log.Printf("[devnet] waiting for TBD to accumulate (30s)...")
	time.Sleep(30 * time.Second)

	// Step 3: transfer TBD from initminer to the first witness.
	witnessName := fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, 1)
	log.Printf("[devnet] transferring TBD to %s...", witnessName)

	ops := []hivego.HiveOperation{
		hivego.TransferOperation{
			From:   "initminer",
			To:     witnessName,
			Amount: "100.000 TBD",
			Memo:   "devnet funding",
		},
		hivego.TransferOperation{
			From:   "initminer",
			To:     witnessName,
			Amount: "1000.000 TESTS",
			Memo:   "devnet funding",
		},
	}
	_, err = hiveClient.Broadcast(ops, &wif)
	if err != nil {
		log.Printf("[devnet] warning: TBD transfer failed (may not have accumulated yet): %v", err)
		log.Printf("[devnet] retrying after 30 more seconds...")
		time.Sleep(30 * time.Second)
		_, err = hiveClient.Broadcast(ops, &wif)
		if err != nil {
			return fmt.Errorf("transferring funds to %s: %w", witnessName, err)
		}
	}

	log.Printf("[devnet] accounts funded")
	return nil
}

// feedPublishOp creates a feed_publish operation as a raw JSON broadcast.
// hivego doesn't have a native FeedPublish struct so we use the generic
// operation interface.
func feedPublishOp(publisher, base, quote string) rawOperation {
	return rawOperation{
		opType: "feed_publish",
		data: map[string]interface{}{
			"publisher": publisher,
			"exchange_rate": map[string]interface{}{
				"base":  base,
				"quote": quote,
			},
		},
	}
}

// rawOperation implements hivego.HiveOperation for arbitrary operations.
type rawOperation struct {
	opType string
	data   map[string]interface{}
}

func (r rawOperation) OpName() string {
	return r.opType
}

func (r rawOperation) SerializeOp() ([]byte, error) {
	return json.Marshal(r.data)
}
