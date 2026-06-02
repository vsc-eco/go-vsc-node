package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// CompressedPubKey is the 33-byte SECP256k1 compressed public key format.
type CompressedPubKey [33]byte

// decodeCompressedPubKey decodes a hex string into a CompressedPubKey,
// validating that it is exactly 33 bytes with a 0x02 or 0x03 prefix.
// Mirrors dash-mapping-contract/contract/mapping/utils.go:DecodeCompressedPubKey.
func decodeCompressedPubKey(hexStr string) (CompressedPubKey, error) {
	var key CompressedPubKey
	b, err := hex.DecodeString(hexStr)
	if err != nil {
		return key, err
	}
	if len(b) != 33 {
		return key, errors.New("invalid compressed public key length: expected 33 bytes")
	}
	if b[0] != 0x02 && b[0] != 0x03 {
		return key, errors.New("invalid compressed public key prefix: expected 0x02 or 0x03")
	}
	copy(key[:], b)
	return key, nil
}

// Backup-path CSV (CheckSequenceVerify) timelock blocks.
// Mainnet: ~1 month at Dash's ~2.5 min block time.
// Testnet: deliberately tiny to exercise the timelock path in tests.
//
// MUST stay in sync with utxo-mapping/dash-mapping-contract/contract/constants/constants.go.
// If these diverge, deposit addresses won't round-trip: the IS service will hand users
// addresses that the contract won't recognise. See the parity test in
// deposit_address_test.go which pins known vectors against the contract's output.
const (
	backupCSVBlocksMainnet = 17280
	backupCSVBlocksTestnet = 2
)

// dashMainNetParams / dashTestNetParams replicate the Dash address params
// from cmd/mapping-bot/chain/dash.go (and lib/dids/dash.go's private copies).
// We need the same encoding the on-chain contract expects.
func dashMainNetParams() *chaincfg.Params {
	p := chaincfg.MainNetParams
	p.PubKeyHashAddrID = 0x4c // 'X' prefix
	p.ScriptHashAddrID = 0x10 // '7' prefix
	p.Bech32HRPSegwit = "dash"
	return &p
}

func dashTestNetParams() *chaincfg.Params {
	p := chaincfg.TestNet3Params
	p.PubKeyHashAddrID = 0x8c // 'y' prefix
	p.ScriptHashAddrID = 0x13 // '8'/'9' prefix
	p.Bech32HRPSegwit = "tdash"
	return &p
}

// createP2WSHAddressWithBackup builds the Dash P2WSH deposit address whose
// redeem script binds (primary spending pubkey, backup pubkey + CSV timelock,
// instruction tag). Faithful port of utxo-mapping/dash-mapping-contract/contract/mapping/utils.go.
//
// Script shape:
//
//	OP_IF
//	    <primaryPubKey>
//	    OP_CHECKSIGVERIFY (or OP_CHECKSIG when tag is empty)
//	    <tag>             (omitted when empty)
//	OP_ELSE
//	    <csvBlocks>
//	    OP_CHECKSEQUENCEVERIFY
//	    OP_DROP
//	    <backupPubKey>
//	    OP_CHECKSIG
//	OP_ENDIF
//
// Returns (bech32 address string, raw witness script bytes, error).
func createP2WSHAddressWithBackup(
	primaryPubKey, backupPubKey CompressedPubKey,
	tag []byte,
	network *chaincfg.Params,
) (string, []byte, error) {
	csvBlocks := backupCSVBlocksMainnet
	if network.Net != chaincfg.MainNetParams.Net {
		csvBlocks = backupCSVBlocksTestnet
	}

	b := txscript.NewScriptBuilder()
	b.AddOp(txscript.OP_IF)

	// Primary spending path.
	b.AddData(primaryPubKey[:])
	if len(tag) > 0 {
		b.AddOp(txscript.OP_CHECKSIGVERIFY)
		b.AddData(tag)
	} else {
		b.AddOp(txscript.OP_CHECKSIG)
	}

	// Backup path (CSV timelock + backup key).
	b.AddOp(txscript.OP_ELSE)
	b.AddInt64(int64(csvBlocks))
	b.AddOp(txscript.OP_CHECKSEQUENCEVERIFY)
	b.AddOp(txscript.OP_DROP)
	b.AddData(backupPubKey[:])
	b.AddOp(txscript.OP_CHECKSIG)

	b.AddOp(txscript.OP_ENDIF)

	script, err := b.Script()
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

// DepositAddress derives the deposit address for a given instruction.
// Tag = SHA256(instruction). Matches the on-chain derivation in
// dash-mapping-contract/contract/mapping/export.go:DepositAddress.
//
// primaryPubKeyHex + backupPubKeyHex are 33-byte compressed pubkeys
// hex-encoded; these come from the bridge TSS-managed key set and are
// constant for the contract's lifetime.
//
// network selects mainnet vs testnet params; the IS service typically
// picks via the -network flag at startup.
func DepositAddress(
	primaryPubKeyHex, backupPubKeyHex, instruction string,
	network *chaincfg.Params,
) (address string, witnessScript []byte, err error) {
	primary, err := decodeCompressedPubKey(primaryPubKeyHex)
	if err != nil {
		return "", nil, fmt.Errorf("primary pubkey: %w", err)
	}
	backup, err := decodeCompressedPubKey(backupPubKeyHex)
	if err != nil {
		return "", nil, fmt.Errorf("backup pubkey: %w", err)
	}
	sum := sha256.Sum256([]byte(instruction))
	return createP2WSHAddressWithBackup(primary, backup, sum[:], network)
}
