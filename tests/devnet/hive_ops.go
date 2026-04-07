package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"

	"github.com/vsc-eco/hivego"
)

// BroadcastCustomJSON broadcasts a custom_json operation to the Hive
// testnet. The operation is signed with the provided WIF key.
func (d *Devnet) BroadcastCustomJSON(id string, requiredAuths []string, payload interface{}, signerWIF string) (string, error) {
	hiveClient := hivego.NewHiveRpc([]string{d.HiveRPCEndpoint()})
	hiveClient.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e" // testnet

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling payload: %w", err)
	}

	op := customJsonOp{
		id:            id,
		requiredAuths: requiredAuths,
		json:          string(jsonBytes),
	}

	txId, err := hiveClient.Broadcast([]hivego.HiveOperation{op}, &signerWIF)
	if err != nil {
		return "", fmt.Errorf("broadcasting %s: %w", id, err)
	}
	return txId, nil
}

// Unstake broadcasts a vsc.consensus_unstake operation for the given
// account. This removes the account's stake after a 5-epoch delay,
// which causes the account to be excluded from future elections once
// its balance drops below MinStake.
func (d *Devnet) Unstake(ctx context.Context, accountName string, amount string) error {
	sysConfig := d.cfg.SysConfigOverrides
	netId := "vsc-devnet"
	if sysConfig != nil && sysConfig.NetId != "" {
		netId = sysConfig.NetId
	}

	payload := map[string]interface{}{
		"from":   "hive:" + accountName,
		"to":     "hive:" + accountName,
		"amount": amount,
		"asset":  "hive",
		"net_id": netId,
	}

	// Use the initminer WIF — devnet-setup creates accounts with the
	// same key as initminer, so this WIF has active authority for all
	// witness accounts.
	wif := d.cfg.InitminerWIF

	log.Printf("[devnet] unstaking %s %s from %s", amount, "TESTS", accountName)
	_, err := d.BroadcastCustomJSON("vsc.consensus_unstake", []string{"hive:" + accountName}, payload, wif)
	return err
}

// customJsonOp implements hivego.HiveOperation for custom_json.
type customJsonOp struct {
	id            string
	requiredAuths []string
	json          string
}

func (c customJsonOp) OpName() string { return "custom_json" }

func (c customJsonOp) marshalData() ([]byte, error) {
	return json.Marshal(map[string]interface{}{
		"id":                     c.id,
		"required_auths":         c.requiredAuths,
		"required_posting_auths": []string{},
		"json":                   c.json,
	})
}

func (c customJsonOp) SerializeOp() ([]byte, error) { return c.marshalData() }
func (c customJsonOp) MarshalJSON() ([]byte, error)  { return c.marshalData() }
