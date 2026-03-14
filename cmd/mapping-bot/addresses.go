package main

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

const BackupCSVBlocks = 4320 // ~1 month

var (
	network   = &chaincfg.TestNet4Params
	pubKeyHex = ""
)

// func makeBtcAddress(instruction string) (string, error) {
// 	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
// 	if err != nil {
// 		return "", err
// 	}

// 	tag := sha256.Sum256([]byte(instruction))

// 	scriptBuilder := txscript.NewScriptBuilder()
// 	scriptBuilder.AddData(pubKeyBytes)              // Push pubkey
// 	scriptBuilder.AddOp(txscript.OP_CHECKSIGVERIFY) // OP_CHECKSIGVERIFY
// 	scriptBuilder.AddData(tag[:])                   // Push tag/bits

// 	script, err := scriptBuilder.Script()
// 	if err != nil {
// 		return "", err
// 	}

// 	witnessProgram := sha256.Sum256(script)
// 	addressWitnessScriptHash, err := btcutil.NewAddressWitnessScriptHash(
// 		witnessProgram[:],
// 		network,
// 	)
// 	if err != nil {
// 		return "", err
// 	}
// 	address := addressWitnessScriptHash.EncodeAddress()

// 	return address, nil
// }

func createP2WSHAddressWithBackup(
	primaryPubKey []byte, backupPubKey []byte, tag []byte, network *chaincfg.Params,
) (string, []byte, error) {
	csvBlocks := BackupCSVBlocks

	if network.Net != chaincfg.MainNetParams.Net {
		csvBlocks = 2
	}

	scriptBuilder := txscript.NewScriptBuilder()

	// start if
	scriptBuilder.AddOp(txscript.OP_IF)

	// primary spending path
	scriptBuilder.AddData(primaryPubKey)
	if tag == nil || len(tag) > 0 {
		scriptBuilder.AddOp(txscript.OP_CHECKSIGVERIFY)
		scriptBuilder.AddData(tag)
	} else {
		scriptBuilder.AddOp(txscript.OP_CHECKSIG)
	}

	// else: backup path
	scriptBuilder.AddOp(txscript.OP_ELSE)

	scriptBuilder.AddInt64(int64(csvBlocks))
	scriptBuilder.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	scriptBuilder.AddOp(txscript.OP_DROP)

	scriptBuilder.AddData(backupPubKey)
	scriptBuilder.AddOp(txscript.OP_CHECKSIG)

	// end if
	scriptBuilder.AddOp(txscript.OP_ENDIF)

	script, err := scriptBuilder.Script()
	if err != nil {
		return "", nil, err
	}

	witnessProgram := sha256.Sum256(script)
	addressWitnessScriptHash, err := btcutil.NewAddressWitnessScriptHash(witnessProgram[:], network)
	if err != nil {
		return "", nil, err
	}

	return addressWitnessScriptHash.EncodeAddress(), script, nil
}

func createP2WSHAddress(pubKeyHex string, tag []byte, network *chaincfg.Params) (string, []byte, error) {
	pubKeyBytes, err := hex.DecodeString(pubKeyHex)
	if err != nil {
		return "", nil, err
	}

	return createSimpleP2WSHAddress(pubKeyBytes, tag, network)
}

func createSimpleP2WSHAddress(pubKeyBytes []byte, tag []byte, network *chaincfg.Params) (string, []byte, error) {
	scriptBuilder := txscript.NewScriptBuilder()
	if len(tag) > 0 {
		scriptBuilder.AddData(pubKeyBytes)
		scriptBuilder.AddOp(txscript.OP_CHECKSIGVERIFY)
		scriptBuilder.AddData(tag)
	} else {
		scriptBuilder.AddData(pubKeyBytes)
		scriptBuilder.AddOp(txscript.OP_CHECKSIG)
	}

	script, err := scriptBuilder.Script()
	if err != nil {
		return "", nil, err
	}

	witnessProgram := sha256.Sum256(script)
	addressWitnessScriptHash, err := btcutil.NewAddressWitnessScriptHash(witnessProgram[:], network)
	if err != nil {
		return "", nil, err
	}
	return addressWitnessScriptHash.EncodeAddress(), script, nil
}
