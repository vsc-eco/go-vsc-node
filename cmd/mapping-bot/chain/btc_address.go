package chain

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// BTCAddressGenerator implements AddressGenerator for Bitcoin (and forks
// like LTC/DASH that use the same P2WSH address format).
type BTCAddressGenerator struct {
	Params          *chaincfg.Params
	BackupCSVBlocks int
}

func (g *BTCAddressGenerator) GenerateDepositAddress(
	primaryPubKeyHex, backupPubKeyHex, instruction string,
) (string, []byte, error) {
	primaryPubKey, err := hex.DecodeString(primaryPubKeyHex)
	if err != nil {
		return "", nil, err
	}
	backupPubKey, err := hex.DecodeString(backupPubKeyHex)
	if err != nil {
		return "", nil, err
	}

	tag := sha256.Sum256([]byte(instruction))
	return createP2WSHWithBackup(primaryPubKey, backupPubKey, tag[:], g.Params, g.BackupCSVBlocks)
}

// createP2WSHWithBackup builds a P2WSH address with a primary key path
// and a backup key path gated by OP_CHECKSEQUENCEVERIFY.
//
// Script:
//
//	OP_IF
//	    <primaryPubKey> OP_CHECKSIGVERIFY <tag>
//	OP_ELSE
//	    <csvBlocks> OP_CHECKSEQUENCEVERIFY OP_DROP <backupPubKey> OP_CHECKSIG
//	OP_ENDIF
func createP2WSHWithBackup(
	primaryPubKey, backupPubKey, tag []byte,
	network *chaincfg.Params,
	csvBlocks int,
) (string, []byte, error) {
	sb := txscript.NewScriptBuilder()

	sb.AddOp(txscript.OP_IF)

	// Primary spending path
	sb.AddData(primaryPubKey)
	if tag != nil && len(tag) > 0 {
		sb.AddOp(txscript.OP_CHECKSIGVERIFY)
		sb.AddData(tag)
	} else {
		sb.AddOp(txscript.OP_CHECKSIG)
	}

	// Backup spending path (CSV timelock)
	sb.AddOp(txscript.OP_ELSE)
	sb.AddInt64(int64(csvBlocks))
	sb.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	sb.AddOp(txscript.OP_DROP)
	sb.AddData(backupPubKey)
	sb.AddOp(txscript.OP_CHECKSIG)

	sb.AddOp(txscript.OP_ENDIF)

	script, err := sb.Script()
	if err != nil {
		return "", nil, err
	}

	witnessProgram := sha256.Sum256(script)
	addr, err := btcutil.NewAddressWitnessScriptHash(witnessProgram[:], network)
	if err != nil {
		return "", nil, err
	}

	return addr.EncodeAddress(), script, nil
}
