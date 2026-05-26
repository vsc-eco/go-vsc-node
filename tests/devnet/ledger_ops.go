package devnet

import (
	"context"
	"fmt"
	"log"

	"github.com/vsc-eco/hivego"
)

// ledger_ops.go — op-submission helpers for the node-wide regression test.
//
// Every VSC ledger op below is a Hive custom_json signed by the initminer key
// (which controls all devnet witness accounts), built on the existing generic
// BroadcastCustomJSON. Deposits are the exception: they are a plain Hive L1
// transfer to the gateway account (NOT a custom_json), mirroring funding.go.
//
// Conventions enforced here (verified against modules/state-processing):
//   - custom_json required_auths use the BARE witness name (e.g. "magi.test1");
//     the state engine prepends "hive:" before checking tx.From
//     (state_engine.go:748-750). Putting "hive:" in required_auths would make
//     Hive reject the op (colons are invalid in account names).
//   - payload "from"/"to" ARE "hive:"-prefixed, matching the post-prefix
//     tx.From the handler compares against.
//   - amounts are Hive-float strings ("5.000"); every vsc.* ledger handler
//     parses them with SafeParseHiveFloat.
//   - net_id must equal the configured VSC network id or the op is rejected
//     (e.g. TxVSCTransfer.ExecuteTx returns "wrong net ID").

// devnetChainID is the Hive testnet chain id used across the devnet harness.
const devnetChainID = "18dcf0a285365fc58b71f18b3d3fec954aa0c141c44e4e5cb4cf777b9eab274e"

// witnessAccount returns the bare witness account name for a 1-indexed node.
func (d *Devnet) witnessAccount(node int) string {
	return fmt.Sprintf("%s%d", d.cfg.WitnessPrefix, node)
}

// netId returns the configured VSC network id (default "vsc-devnet").
func (d *Devnet) netId() string {
	if d.cfg.SysConfigOverrides != nil && d.cfg.SysConfigOverrides.NetId != "" {
		return d.cfg.SysConfigOverrides.NetId
	}
	return "vsc-devnet"
}

// hiveAssetSymbol maps a VSC asset ("hive"/"hbd") to the Hive testnet symbol.
// On the Hive testnet, TESTS carries NAI @@000000021 (-> "hive") and TBD
// carries @@000000013 (-> "hbd"), which the gateway deposit handler keys on.
func hiveAssetSymbol(asset string) string {
	if asset == "hbd" {
		return "TBD"
	}
	return "TESTS"
}

// Deposit moves funds from a witness's Hive L1 balance into the VSC ledger by
// transferring to the gateway account. asset is "hive" or "hbd"; amount is a
// Hive-float string like "5.000". Returns the L1 transaction id.
func (d *Devnet) Deposit(ctx context.Context, fromWitness int, amount, asset string) (string, error) {
	from := d.witnessAccount(fromWitness)
	op := hivego.TransferOperation{
		From:   from,
		To:     "vsc.gateway",
		Amount: amount + " " + hiveAssetSymbol(asset),
		Memo:   "regression deposit",
	}
	hiveClient := hivego.NewHiveRpc([]string{d.DroneEndpoint()})
	hiveClient.ChainID = devnetChainID
	wif := d.cfg.InitminerWIF
	txId, err := hiveClient.Broadcast([]hivego.HiveOperation{op}, &wif)
	if err != nil {
		return "", fmt.Errorf("deposit %s %s from %s: %w", amount, asset, from, err)
	}
	log.Printf("[devnet] deposit tx=%s %s %s from %s", txId, amount, asset, from)
	return txId, nil
}

// ledgerOp is the shared shape for the vsc.* from/to/amount/asset/net_id ops.
// fromAcct/toAcct are bare account names; they are "hive:"-prefixed in the
// payload, while required_auths uses the bare name (the state engine prepends
// "hive:" before checking tx.From).
func (d *Devnet) ledgerOp(id, fromAcct, toAcct, amount, asset, memo string) (string, error) {
	payload := map[string]interface{}{
		"from":   "hive:" + fromAcct,
		"to":     "hive:" + toAcct,
		"amount": amount,
		"asset":  asset,
		"net_id": d.netId(),
	}
	if memo != "" {
		payload["memo"] = memo
	}
	txId, err := d.BroadcastCustomJSON(id, []string{fromAcct}, payload, d.cfg.InitminerWIF)
	if err != nil {
		return "", fmt.Errorf("%s from %s: %w", id, fromAcct, err)
	}
	log.Printf("[devnet] %s tx=%s %s %s %s->%s", id, txId, amount, asset, fromAcct, toAcct)
	return txId, nil
}

// Transfer broadcasts vsc.transfer between two witness accounts' VSC balances.
func (d *Devnet) Transfer(fromWitness, toWitness int, amount, asset, memo string) (string, error) {
	return d.ledgerOp("vsc.transfer", d.witnessAccount(fromWitness), d.witnessAccount(toWitness), amount, asset, memo)
}

// Withdraw broadcasts vsc.withdraw, moving a witness's VSC balance back to a
// Hive L1 account (toL1, a bare account name).
func (d *Devnet) Withdraw(fromWitness int, toL1, amount, asset, memo string) (string, error) {
	return d.ledgerOp("vsc.withdraw", d.witnessAccount(fromWitness), toL1, amount, asset, memo)
}

// StakeHBD broadcasts vsc.stake_hbd, moving HBD into savings (interest-bearing).
func (d *Devnet) StakeHBD(fromWitness, toWitness int, amount string) (string, error) {
	return d.ledgerOp("vsc.stake_hbd", d.witnessAccount(fromWitness), d.witnessAccount(toWitness), amount, "hbd", "")
}

// UnstakeHBD broadcasts vsc.unstake_hbd, pulling HBD back out of savings.
func (d *Devnet) UnstakeHBD(fromWitness, toWitness int, amount string) (string, error) {
	return d.ledgerOp("vsc.unstake_hbd", d.witnessAccount(fromWitness), d.witnessAccount(toWitness), amount, "hbd", "")
}

// ConsensusStake broadcasts vsc.consensus_stake, adding HIVE consensus weight.
// This is additive and never removes a witness from elections.
func (d *Devnet) ConsensusStake(fromWitness, toWitness int, amount string) (string, error) {
	return d.ledgerOp("vsc.consensus_stake", d.witnessAccount(fromWitness), d.witnessAccount(toWitness), amount, "hive", "")
}

// ConsensusUnstake broadcasts vsc.consensus_unstake for a witness, removing
// consensus stake after the protocol's unstaking delay. Dropping a witness's
// stake below MinStake removes it from future elections. This is the correct
// (bare-required-auths) form; prefer it over the legacy Unstake helper.
func (d *Devnet) ConsensusUnstake(witness int, amount string) (string, error) {
	acct := d.witnessAccount(witness)
	return d.ledgerOp("vsc.consensus_unstake", acct, acct, amount, "hive", "")
}
