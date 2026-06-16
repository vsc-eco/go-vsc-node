package devnet

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/vsc-eco/hivego"
)

// FundL2Account credits an L2 account (Hive `hive:<account>` or
// did:pkh:eip155:1:0x...) with HBD by broadcasting a Hive
// TransferOperation from initminer to the VSC GatewayWallet with the
// `to=<l2-account>` memo. The state engine recognises the memo when
// the receiver is the gateway wallet and credits the destination on
// L2 (see modules/state-processing — covered by ledger_system_test.go's
// "memo to=<eth address>" case).
//
// hbdAmount is the decimal-string amount; devnet uses Hive's testnet
// asset symbol set (TBD for HBD). Block time on devnet is 3s; the
// helper sleeps `settleWait` to give the L2 a chance to credit before
// returning. The IS service's L2 submitter does its own nonce-fetch
// before broadcasting so an unfunded-at-submission-time account would
// still fail, even though the deposit broadcast succeeded earlier.
//
// Used by the IS-login devnet smoke test to fund the IS service's
// derived did:pkh:eip155 account before switching to the real L2
// submitter (otherwise the submitter would abort on first call with
// "insufficient RC" because the account has no HBD-backed L2 balance).
func (d *Devnet) FundL2Account(
	ctx context.Context,
	l2DID string,
	hbdAmount string,
	settleWait time.Duration,
) error {
	hiveClient := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	hiveClient.ChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"
	wif := d.cfg.InitminerWIF

	op := hivego.TransferOperation{
		From:   "initminer",
		To:     "vsc.gateway",
		Amount: hbdAmount + " TBD",
		Memo:   "to=" + l2DID,
	}
	log.Printf("[devnet] L1→L2 deposit: initminer → vsc.gateway (%s TBD, memo=%q)",
		hbdAmount, op.Memo)
	txid, err := hiveClient.Broadcast([]hivego.HiveOperation{op}, &wif)
	if err != nil {
		return fmt.Errorf("L1→L2 deposit broadcast: %w", err)
	}
	log.Printf("[devnet] L1→L2 deposit tx broadcast: %s", txid)

	select {
	case <-time.After(settleWait):
	case <-ctx.Done():
		return ctx.Err()
	}
	log.Printf("[devnet] L1→L2 settle wait (%s) complete; L2 should now have credited %s",
		settleWait, l2DID)
	return nil
}
