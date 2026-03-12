package dids

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	blocks "github.com/ipfs/go-block-format"
)

// ===== constants =====

// did:pkh:bip122 method for Bitcoin mainnet
// CAIP-2 chain ID: first 32 hex chars of genesis block hash
const BtcDIDPrefix = "did:pkh:bip122:000000000019d6689c085ae165831e93/"

// ===== interface assertions =====

var _ DID = BtcDID("")

// ===== BtcDID =====

type BtcDID string

func ParseBtcDID(did string) (BtcDID, error) {
	addr, hasPrefix := strings.CutPrefix(did, BtcDIDPrefix)
	if !hasPrefix {
		return "", fmt.Errorf("does not have btc prefix")
	}

	if strings.HasPrefix(addr, "bc1p") {
		return "", fmt.Errorf("taproot (P2TR) addresses not supported, Phase 2")
	}

	decoded, err := btcutil.DecodeAddress(addr, &chaincfg.MainNetParams)
	if err != nil {
		return "", fmt.Errorf("invalid Bitcoin address: %w", err)
	}

	switch decoded.(type) {
	case *btcutil.AddressPubKeyHash, *btcutil.AddressScriptHash, *btcutil.AddressWitnessPubKeyHash:
		// supported
	default:
		return "", fmt.Errorf("unsupported Bitcoin address type: %T", decoded)
	}

	return BtcDID(did), nil
}

func NewBtcDID(btcAddr string) BtcDID {
	return BtcDID(BtcDIDPrefix + btcAddr)
}

// ===== implementing the DID interface =====

func (d BtcDID) String() string {
	return string(d)
}

func (d BtcDID) Identifier() string {
	return string(d)[len(BtcDIDPrefix):]
}

func (d BtcDID) Verify(data blocks.Block, sig string) (bool, error) {
	addr := d.Identifier()

	addrType, err := classifyBtcAddress(addr)
	if err != nil {
		return false, fmt.Errorf("unsupported address: %w", err)
	}

	// decode base64 signature
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}

	// BIP-137 compact signature (65 bytes)
	if len(sigBytes) == 65 {
		return verifyBIP137(addr, addrType, data, sigBytes)
	}

	// BIP-322 "simple" signature (variable length, typically ~107 bytes for P2WPKH)
	if addrType == "p2wpkh" {
		return verifyBIP322Simple(addr, data, sigBytes)
	}

	return false, fmt.Errorf("unsupported signature length: %d (expected 65 for BIP-137 or 107 for BIP-322 P2WPKH)", len(sigBytes))
}

// ===== BIP-137 verification =====

func verifyBIP137(addr string, addrType string, data blocks.Block, sigBytes []byte) (bool, error) {
	hash := BitcoinMessageHash(data.Cid().String())

	// recover public key — RecoverCompact handles the header byte internally
	pubKey, _, err := ecdsa.RecoverCompact(sigBytes, hash)
	if err != nil {
		return false, fmt.Errorf("failed to recover public key: %w", err)
	}

	// derive address from recovered key based on the DID's address type
	switch addrType {
	case "p2pkh":
		// try compressed key first
		compHash := btcutil.Hash160(pubKey.SerializeCompressed())
		compAddr, err := btcutil.NewAddressPubKeyHash(compHash, &chaincfg.MainNetParams)
		if err != nil {
			return false, err
		}
		if compAddr.String() == addr {
			return true, nil
		}
		// try uncompressed key (legacy P2PKH supports both)
		uncompHash := btcutil.Hash160(pubKey.SerializeUncompressed())
		uncompAddr, err := btcutil.NewAddressPubKeyHash(uncompHash, &chaincfg.MainNetParams)
		if err != nil {
			return false, err
		}
		return uncompAddr.String() == addr, nil

	case "p2sh":
		// P2SH-P2WPKH requires compressed key
		pubKeyHash := btcutil.Hash160(pubKey.SerializeCompressed())
		redeemScript := append([]byte{0x00, 0x14}, pubKeyHash...)
		derived, err := btcutil.NewAddressScriptHash(redeemScript, &chaincfg.MainNetParams)
		if err != nil {
			return false, err
		}
		return derived.String() == addr, nil

	case "p2wpkh":
		// native segwit requires compressed key
		pubKeyHash := btcutil.Hash160(pubKey.SerializeCompressed())
		derived, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
		if err != nil {
			return false, err
		}
		return derived.String() == addr, nil
	}

	return false, fmt.Errorf("unknown address type")
}

// ===== BIP-322 "simple" verification =====

// verifyBIP322Simple verifies a BIP-322 "simple" signature for P2WPKH addresses.
// BIP-322 signs a virtual transaction sighash (BIP-143), NOT the Bitcoin message hash.
// Leather wallet returns BIP-322 for all SegWit (bc1q) addresses.
func verifyBIP322Simple(addr string, data blocks.Block, sigBytes []byte) (bool, error) {
	// 1. Parse witness stack: [num_items, sig_len, sig_bytes, pubkey_len, pubkey_bytes]
	items, err := parseCompactWitnessStack(sigBytes)
	if err != nil {
		return false, fmt.Errorf("BIP-322: %w", err)
	}
	if len(items) != 2 {
		return false, fmt.Errorf("BIP-322: expected 2 witness items, got %d", len(items))
	}

	sigWithSighash := items[0]
	pubkeyBytes := items[1]

	// 2. Parse and validate compressed public key
	if len(pubkeyBytes) != 33 {
		return false, fmt.Errorf("BIP-322: expected 33-byte compressed pubkey, got %d", len(pubkeyBytes))
	}
	pubkey, err := btcec.ParsePubKey(pubkeyBytes)
	if err != nil {
		return false, fmt.Errorf("BIP-322: invalid public key: %w", err)
	}

	// 3. Verify public key matches the P2WPKH address
	pubkeyHash := btcutil.Hash160(pubkey.SerializeCompressed())
	derivedAddr, err := btcutil.NewAddressWitnessPubKeyHash(pubkeyHash, &chaincfg.MainNetParams)
	if err != nil {
		return false, fmt.Errorf("BIP-322: failed to derive address: %w", err)
	}
	if derivedAddr.String() != addr {
		return false, nil // pubkey doesn't match address
	}

	// 4. Compute BIP-322 tagged message hash
	msgHash := bip322TaggedHash([]byte(data.Cid().String()))

	// 5. Build scriptPubKey for the P2WPKH address
	decoded, err := btcutil.DecodeAddress(addr, &chaincfg.MainNetParams)
	if err != nil {
		return false, fmt.Errorf("BIP-322: failed to decode address: %w", err)
	}
	scriptPubKey, err := txscript.PayToAddrScript(decoded)
	if err != nil {
		return false, fmt.Errorf("BIP-322: failed to build scriptPubKey: %w", err)
	}

	// 6. Build the "to_spend" virtual transaction (BIP-322 spec)
	toSpend := wire.NewMsgTx(0)
	scriptSig := make([]byte, 0, 34)
	scriptSig = append(scriptSig, txscript.OP_0)
	scriptSig = append(scriptSig, txscript.OP_DATA_32)
	scriptSig = append(scriptSig, msgHash...)
	nullHash := chainhash.Hash{}
	txIn := wire.NewTxIn(wire.NewOutPoint(&nullHash, 0xFFFFFFFF), scriptSig, nil)
	txIn.Sequence = 0
	toSpend.AddTxIn(txIn)
	toSpend.AddTxOut(wire.NewTxOut(0, scriptPubKey))

	// 7. Build the "to_sign" virtual transaction
	toSpendHash := toSpend.TxHash()
	toSign := wire.NewMsgTx(0)
	toSignIn := wire.NewTxIn(wire.NewOutPoint(&toSpendHash, 0), nil, nil)
	toSignIn.Sequence = 0
	toSign.AddTxIn(toSignIn)
	toSign.AddTxOut(wire.NewTxOut(0, []byte{txscript.OP_RETURN}))

	// 8. Compute BIP-143 segwit sighash
	// For P2WPKH, the scriptCode is the equivalent P2PKH script
	scriptCode, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_DUP).
		AddOp(txscript.OP_HASH160).
		AddData(pubkeyHash).
		AddOp(txscript.OP_EQUALVERIFY).
		AddOp(txscript.OP_CHECKSIG).
		Script()
	if err != nil {
		return false, fmt.Errorf("BIP-322: failed to build scriptCode: %w", err)
	}

	// Determine sighash type from the trailing byte after the DER signature
	sighashType := txscript.SigHashAll
	if len(sigWithSighash) > 2 && sigWithSighash[0] == 0x30 {
		derLen := int(sigWithSighash[1]) + 2
		if len(sigWithSighash) > derLen {
			sighashType = txscript.SigHashType(sigWithSighash[derLen])
		}
	}

	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(scriptPubKey, 0)
	sigHashes := txscript.NewTxSigHashes(toSign, prevOutputFetcher)
	sighashBytes, err := txscript.CalcWitnessSigHash(scriptCode, sigHashes, sighashType, toSign, 0, 0)
	if err != nil {
		return false, fmt.Errorf("BIP-322: failed to compute sighash: %w", err)
	}

	// 9. Parse DER signature (strip sighash type byte) and verify
	derSig := stripSighashType(sigWithSighash)
	parsedSig, err := ecdsa.ParseDERSignature(derSig)
	if err != nil {
		return false, fmt.Errorf("BIP-322: failed to parse DER signature: %w", err)
	}

	return parsedSig.Verify(sighashBytes, pubkey), nil
}

// bip322TaggedHash computes the BIP-322 tagged message hash.
// SHA256(SHA256("BIP0322-signed-message") || SHA256("BIP0322-signed-message") || message)
func bip322TaggedHash(message []byte) []byte {
	tag := sha256.Sum256([]byte("BIP0322-signed-message"))
	h := sha256.New()
	h.Write(tag[:])
	h.Write(tag[:])
	h.Write(message)
	result := h.Sum(nil)
	return result
}

// parseCompactWitnessStack parses a serialized witness stack.
// Format: varint(num_items) [varint(item_len) item_bytes]...
func parseCompactWitnessStack(data []byte) ([][]byte, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty witness data")
	}
	offset := 0
	numItems := int(data[offset])
	offset++

	items := make([][]byte, 0, numItems)
	for i := 0; i < numItems; i++ {
		if offset >= len(data) {
			return nil, fmt.Errorf("unexpected end of witness data at item %d", i)
		}
		itemLen := int(data[offset])
		offset++
		if offset+itemLen > len(data) {
			return nil, fmt.Errorf("witness item %d length %d exceeds data bounds", i, itemLen)
		}
		items = append(items, data[offset:offset+itemLen])
		offset += itemLen
	}
	return items, nil
}

// stripSighashType removes the trailing sighash type byte from a DER+sighash signature.
func stripSighashType(sigWithSighash []byte) []byte {
	if len(sigWithSighash) < 3 || sigWithSighash[0] != 0x30 {
		return sigWithSighash
	}
	derLen := int(sigWithSighash[1]) + 2
	if len(sigWithSighash) > derLen {
		return sigWithSighash[:derLen]
	}
	return sigWithSighash
}

// ===== helpers =====

// BitcoinMessageHash computes the double-SHA256 hash of a Bitcoin Signed Message.
// Format: \x18"Bitcoin Signed Message:\n" + varint(len(msg)) + msg
func BitcoinMessageHash(msg string) []byte {
	prefix := []byte("\x18Bitcoin Signed Message:\n")
	vi := writeVarInt(len(msg))
	buf := make([]byte, 0, len(prefix)+len(vi)+len(msg))
	buf = append(buf, prefix...)
	buf = append(buf, vi...)
	buf = append(buf, []byte(msg)...)
	first := sha256.Sum256(buf)
	second := sha256.Sum256(first[:])
	return second[:]
}

func writeVarInt(n int) []byte {
	if n < 0xFD {
		return []byte{byte(n)}
	}
	if n <= 0xFFFF {
		return []byte{0xFD, byte(n), byte(n >> 8)}
	}
	if n <= 0xFFFFFFFF {
		return []byte{0xFE, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}
	}
	return []byte{0xFF, byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24),
		byte(n >> 32), byte(n >> 40), byte(n >> 48), byte(n >> 56)}
}

func classifyBtcAddress(addr string) (string, error) {
	decoded, err := btcutil.DecodeAddress(addr, &chaincfg.MainNetParams)
	if err != nil {
		return "", err
	}
	switch decoded.(type) {
	case *btcutil.AddressPubKeyHash:
		return "p2pkh", nil
	case *btcutil.AddressScriptHash:
		return "p2sh", nil
	case *btcutil.AddressWitnessPubKeyHash:
		return "p2wpkh", nil
	default:
		return "", fmt.Errorf("unsupported Bitcoin address type: %T", decoded)
	}
}
