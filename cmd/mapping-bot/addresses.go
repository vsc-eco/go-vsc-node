package main

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

var (
	network   = &chaincfg.TestNet3Params
	pubKeyHex = ""
)

func makeBtcAddress(vscAddr string) (string, error) {
	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return "", err
	}

	tag := sha256.Sum256([]byte(vscAddr))

	scriptBuilder := txscript.NewScriptBuilder()
	scriptBuilder.AddData(pubKeyBytes)              // Push pubkey
	scriptBuilder.AddOp(txscript.OP_CHECKSIGVERIFY) // OP_CHECKSIGVERIFY
	scriptBuilder.AddData(tag[:])                   // Push tag/bits

	script, err := scriptBuilder.Script()
	if err != nil {
		return "", err
	}

	witnessProgram := sha256.Sum256(script)
	addressWitnessScriptHash, err := btcutil.NewAddressWitnessScriptHash(
		witnessProgram[:],
		network,
	)
	if err != nil {
		return "", err
	}
	address := addressWitnessScriptHash.EncodeAddress()

	return address, nil
}
