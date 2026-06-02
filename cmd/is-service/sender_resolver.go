package main

import (
	"bytes"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
)

// resolveSenderAddress parses raw Dash tx bytes (hex) and returns the
// single L1 address that signed all inputs.
//
// Mirrors dash-mapping-contract/contract/mapping/forwarder_integration.go's
// resolveDashDIDFromTxInputs — enforces the strict "all inputs spend from
// the same address" rule (spec §5.2.5). On multi-address inputs returns
// an error; the contract would reject the tx anyway, so we save a round
// trip by refusing to surface a session it would later fail.
func resolveSenderAddress(rawTxHex string, netParams *chaincfg.Params) (string, error) {
	rawTxBytes, err := hex.DecodeString(rawTxHex)
	if err != nil {
		return "", fmt.Errorf("raw tx not hex: %w", err)
	}
	var msgTx wire.MsgTx
	if err := msgTx.Deserialize(bytes.NewReader(rawTxBytes)); err != nil {
		return "", fmt.Errorf("raw tx parse: %w", err)
	}
	if len(msgTx.TxIn) == 0 {
		return "", fmt.Errorf("tx has no inputs")
	}
	var sender string
	for i, in := range msgTx.TxIn {
		pkScript, err := txscript.ComputePkScript(in.SignatureScript, in.Witness)
		if err != nil {
			return "", fmt.Errorf("input %d: cannot compute pkScript: %w", i, err)
		}
		_, addrs, _, err := txscript.ExtractPkScriptAddrs(pkScript.Script(), netParams)
		if err != nil || len(addrs) == 0 {
			return "", fmt.Errorf("input %d: cannot extract address", i)
		}
		addr := addrs[0].EncodeAddress()
		if sender == "" {
			sender = addr
		} else if sender != addr {
			return "", fmt.Errorf("multi-address inputs: %s vs %s (§5.2.5 strict rule)", sender, addr)
		}
	}
	if sender == "" {
		return "", fmt.Errorf("could not resolve sender address")
	}
	return sender, nil
}
