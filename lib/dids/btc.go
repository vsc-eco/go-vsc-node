package dids

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/btcutil"
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

	// hash the block's CID string using Bitcoin Signed Message format
	hash := BitcoinMessageHash(data.Cid().String())

	// decode base64 BIP-137 signature
	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}
	if len(sigBytes) != 65 {
		return false, fmt.Errorf("invalid signature length: expected 65, got %d", len(sigBytes))
	}

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
