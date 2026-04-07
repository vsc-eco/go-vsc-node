package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/vsc-eco/hivego"
)

// CallContract broadcasts a Hive custom_json that invokes a contract
// method. The caller is the witness at the given node index (1-indexed).
// Retries up to 3 times on transient connection errors.
func (d *Devnet) CallContract(ctx context.Context, node int, contractId, action, payload string) (string, error) {
	witnessName := fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, node)

	callTx := map[string]interface{}{
		"net_id":      "vsc-devnet",
		"contract_id": contractId,
		"action":      action,
		"payload":     payload,
		"rc_limit":    500000,
		"intents":     []interface{}{},
	}
	txJSON, err := json.Marshal(callTx)
	if err != nil {
		return "", fmt.Errorf("marshaling call tx: %w", err)
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        []string{witnessName},
		RequiredPostingAuths: []string{},
		Id:                   "vsc.call",
		Json:                 string(txJSON),
	}

	wif := d.cfg.InitminerWIF

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			log.Printf("[devnet] retrying contract call (attempt %d)...", attempt+1)
			time.Sleep(3 * time.Second)
		}
		hiveClient := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
		hiveClient.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"

		txId, err := hiveClient.Broadcast([]hivego.HiveOperation{op}, &wif)
		if err == nil {
			log.Printf("[devnet] contract call tx=%s action=%s on %s", txId, action, contractId)
			return txId, nil
		}
		lastErr = err
	}
	return "", fmt.Errorf("broadcasting contract call: %w", lastErr)
}
