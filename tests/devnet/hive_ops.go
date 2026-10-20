package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/vsc-eco/hivego"
)

// CallContract broadcasts a Hive custom_json that invokes a contract
// method. The caller is the witness at the given node index (1-indexed).
// Retries up to 3 times on transient connection errors.
func (d *Devnet) CallContract(ctx context.Context, node int, contractId, action, payload string) (string, error) {
	return d.CallContractWithIntents(ctx, node, contractId, action, payload, nil, 500000)
}

// CallContractWithIntents is CallContract plus an `intents` array (e.g. a
// transfer.allow intent that authorizes the contract to hive.draw a token from
// the caller) and an explicit rc_limit. Each intent is
// {"type": "...", "args": {"k":"v"}}.
//
// rc_limit matters for intent draws: PullBalance reserves
// (rc_limit - free_rc) HBD of the caller's balance against RC, so a call that
// draws HBD must use a LOW rc_limit (<= RC_HIVE_FREE_AMOUNT) or the caller's
// balance is reserved out from under the draw (see [[vsc_rc_limit_hbd_exclusion]]).
func (d *Devnet) CallContractWithIntents(ctx context.Context, node int, contractId, action, payload string, intents []map[string]interface{}, rcLimit int) (string, error) {
	witnessName := fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, node)

	intentsField := make([]interface{}, 0, len(intents))
	for _, it := range intents {
		intentsField = append(intentsField, it)
	}

	callTx := map[string]interface{}{
		"net_id":      "vsc-devnet",
		"contract_id": contractId,
		"action":      action,
		"payload":     payload,
		"rc_limit":    rcLimit,
		"intents":     intentsField,
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
			devnetSleep(3 * time.Second)
		}
		hiveClient := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
		hiveClient.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"

		txId, err := hiveClient.Broadcast([]hivego.HiveOperation{op}, &wif)
		if err == nil {
			log.Printf("[devnet] contract call tx=%s action=%s on %s", txId, action, contractId)
			return txId, nil
		}
		log.Printf("[devnet] contract call action=%s attempt %d failed: %v", action, attempt+1, err)
		lastErr = err
		// Resend only what the server never read. Anything else may already be
		// on chain, and a re-signed resend would apply the call twice (H-15).
		if !broadcastNeverReachedServer(err) {
			break
		}
	}
	return "", fmt.Errorf("broadcasting contract call: %w", lastErr)
}

// BroadcastCustomJSON broadcasts a custom_json operation to the Hive
// testnet. The operation is signed with the provided WIF key.
func (d *Devnet) BroadcastCustomJSON(id string, requiredAuths []string, payload interface{}, signerWIF string) (string, error) {
	hiveClient := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	hiveClient.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"

	jsonBytes, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling payload: %w", err)
	}

	op := hivego.CustomJsonOperation{
		RequiredAuths:        requiredAuths,
		RequiredPostingAuths: []string{},
		Id:                   id,
		Json:                 string(jsonBytes),
	}

	txId, err := broadcastRetryingStale(hiveClient, []hivego.HiveOperation{op}, &signerWIF, id)
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

	wif := d.cfg.InitminerWIF

	log.Printf("[devnet] unstaking %s %s from %s", amount, "TESTS", accountName)
	// required_auths takes the bare Hive account (as ledgerOp does): hived has no
	// account named "hive:magi.test1" and refuses the whole transaction.
	_, err := d.BroadcastCustomJSON("vsc.consensus_unstake", []string{accountName}, payload, wif)
	return err
}

// broadcastRetryingStale broadcasts ops, retrying only a keep-alive connection
// the server had already closed ("closed connection before returning the first
// response byte"): that one fails before the request is read, so nothing reached
// Hive. Any other failure may have, and a re-signed retry would broadcast the
// operations twice. Every harness broadcast that has no retry of its own goes
// through here (H-7: P11 and FDV10 lost TestConsensusDelegatedStakeDevnet to
// this error on two different broadcasts).
func broadcastRetryingStale(client *hivego.HiveRpcNode, ops []hivego.HiveOperation, wif *string, what string) (string, error) {
	txId, err := client.Broadcast(ops, wif)
	for attempt := 1; attempt <= 2 && staleKeepAlive(err); attempt++ {
		log.Printf("[devnet] %s: stale keep-alive connection, rebroadcasting (retry %d)", what, attempt)
		txId, err = client.Broadcast(ops, wif)
	}
	return txId, err
}

// staleKeepAlive reports a keep-alive connection the server had already closed:
// the request failed before the server read it.
func staleKeepAlive(err error) bool {
	return err != nil && strings.Contains(err.Error(), "closed connection before returning the first response byte")
}

// broadcastNeverReachedServer reports a broadcast failure from before the server
// read the request, so a resend cannot apply it twice: a stale keep-alive, or no
// connection at all. In 125 contract calls of FDV10R FullNetworkRegression both
// failed first attempts were stale keep-alives.
func broadcastNeverReachedServer(err error) bool {
	return staleKeepAlive(err) || (err != nil && strings.Contains(err.Error(), "connect: connection refused"))
}
