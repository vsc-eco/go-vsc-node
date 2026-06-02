package dids

import (
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	blocks "github.com/ipfs/go-block-format"
)

// ===== constants =====

// did:pkh:bip122 method for Dash mainnet.
// CAIP-2 chain ID: first 32 hex chars of mainnet genesis hash
//
//	00000ffd590b1485b3caadc19b22e6379c733355108f107a430458cdf3407ab6
const DashDIDPrefix = "did:pkh:bip122:00000ffd590b1485b3caadc19b22e637:"

// did:pkh:bip122 method for Dash testnet.
// CAIP-2 chain ID: first 32 hex chars of testnet genesis hash
//
//	00000bafbc94add76cb75e2ec92894837288a481e5c005f6563d91623bf8bc2c
const DashTestnetDIDPrefix = "did:pkh:bip122:00000bafbc94add76cb75e2ec9289483:"

// DashSignedMessageMagic is the magic-prefix string Dash Core's `signmessage`
// RPC and GUI use when constructing the BIP-137 message digest. This is
// historic: Dash forked from Bitcoin while still called "DarkCoin" and the
// prefix was never updated. Confirmed in `src/util/message.cpp` of the
// dashpay/dash repository and DIP-0003.
//
// Using the wrong prefix (e.g. "Dash Signed Message:\n") silently makes
// every valid Dash signature fail to verify.
const DashSignedMessageMagic = "DarkCoin Signed Message:\n"

// DashISLockDomainPrefix is the domain-separation tag used when validators
// sign IS-lock attestations with their consensus BLS key. Prevents
// cross-domain signature confusion with block signing (which uses raw CIDs
// of CBOR-encoded structures). Mirrors the BLS PoP precedent
// ("VSC-BLS-POP-v1" in bls.go:185).
//
// Canonical IS-lock attestation message:
//
//	H(DashISLockDomainPrefix || chainID || epoch || txid || rawTxHashRaw || instructionHashRaw)
const DashISLockDomainPrefix = "dash-is-lock-v1\x00"

// ===== Dash address params =====

// dashMainNetParams returns btcutil-compatible chain parameters for Dash
// mainnet address encoding. We start from Bitcoin's MainNetParams and
// override the prefix bytes — Dash's address format is otherwise identical
// to Bitcoin's at the encoding layer.
//
// Mirror of cmd/mapping-bot/chain/dash.go's dashMainNetParams. Keep these
// in sync — if the mapping bot's encoding diverges from this DID, addresses
// won't round-trip.
func dashMainNetParams() *chaincfg.Params {
	p := chaincfg.MainNetParams
	p.PubKeyHashAddrID = 0x4c // 'X' prefix
	p.ScriptHashAddrID = 0x10 // '7' prefix
	p.Bech32HRPSegwit = "dash"
	return &p
}

// dashTestNetParams returns btcutil-compatible chain parameters for Dash
// testnet address encoding. Mirror of cmd/mapping-bot/chain/dash.go.
func dashTestNetParams() *chaincfg.Params {
	p := chaincfg.TestNet3Params
	p.PubKeyHashAddrID = 0x8c // 'y' prefix
	p.ScriptHashAddrID = 0x13 // '8'/'9' prefix
	p.Bech32HRPSegwit = "tdash"
	return &p
}

// ===== interface assertions =====

var _ DID = DashDID("")

// ===== DashDID =====

// DashDID is a Dash address identity expressed as a did:pkh:bip122 string.
// Both mainnet and testnet are represented by the same Go type — the
// chain is disambiguated by the prefix the string carries.
type DashDID string

// NewDashDID constructs a mainnet DashDID. Does NOT validate the address;
// use ParseDashDID after if you need validation.
func NewDashDID(dashAddr string) DashDID {
	return DashDID(DashDIDPrefix + dashAddr)
}

// NewDashTestnetDID constructs a testnet DashDID.
func NewDashTestnetDID(dashAddr string) DashDID {
	return DashDID(DashTestnetDIDPrefix + dashAddr)
}

// ParseDashDID parses and validates a mainnet Dash DID. Returns the typed
// DashDID on success, or an error if the prefix or address is wrong.
//
// Accepted address types: P2PKH ('X' prefix) and P2SH ('7' prefix).
// Native segwit (P2WPKH) is intentionally not supported — Dash's
// mainstream wallets don't deploy bech32, and btcutil's bech32 decoder
// requires global HRP registration that would have side effects across
// other consumers of the library.
func ParseDashDID(did string) (DashDID, error) {
	addr, hasPrefix := strings.CutPrefix(did, DashDIDPrefix)
	if !hasPrefix {
		return "", fmt.Errorf("does not have dash prefix")
	}

	if err := validateDashAddress(addr, dashMainNetParams()); err != nil {
		return "", err
	}

	return DashDID(did), nil
}

// ParseDashTestnetDID parses and validates a testnet Dash DID.
func ParseDashTestnetDID(did string) (DashDID, error) {
	addr, hasPrefix := strings.CutPrefix(did, DashTestnetDIDPrefix)
	if !hasPrefix {
		return "", fmt.Errorf("does not have dash testnet prefix")
	}

	if err := validateDashAddress(addr, dashTestNetParams()); err != nil {
		return "", err
	}

	return DashDID(did), nil
}

// validateDashAddress checks that addr decodes under the given params and
// is one of the supported address types (P2PKH or P2SH).
func validateDashAddress(addr string, params *chaincfg.Params) error {
	decoded, err := btcutil.DecodeAddress(addr, params)
	if err != nil {
		return fmt.Errorf("invalid Dash address: %w", err)
	}

	switch decoded.(type) {
	case *btcutil.AddressPubKeyHash, *btcutil.AddressScriptHash:
		return nil
	default:
		return fmt.Errorf("unsupported Dash address type: %T", decoded)
	}
}

// ===== implementing the DID interface =====

func (d DashDID) String() string {
	return string(d)
}

// Identifier returns the bare Dash address (without the DID prefix).
func (d DashDID) Identifier() string {
	if strings.HasPrefix(string(d), DashDIDPrefix) {
		return string(d)[len(DashDIDPrefix):]
	}
	if strings.HasPrefix(string(d), DashTestnetDIDPrefix) {
		return string(d)[len(DashTestnetDIDPrefix):]
	}
	// Malformed DID — return raw string. Callers that care should use
	// ParseDashDID / ParseDashTestnetDID instead.
	return string(d)
}

// IsTestnet reports whether this DID is in the Dash testnet namespace.
func (d DashDID) IsTestnet() bool {
	return strings.HasPrefix(string(d), DashTestnetDIDPrefix)
}

// Verify validates a BIP-137 "DarkCoin Signed Message" signature against
// this DashDID's address. The signature is expected to be base64-encoded;
// the message is the CID of the block as a string.
//
// Dash Core (and other Dash desktop wallets) produce these via the
// `signmessage` RPC. Mobile wallets (DashPay, Edge, etc.) typically do
// not expose a signing UI — the broader Dash InstantSend login feature
// works around this by using the IS payment itself as the signature
// (handled outside this DID type).
//
// Verify supports the same address types as ParseDashDID: P2PKH and
// P2SH-P2WPKH. Native segwit (P2WPKH) is not supported (see ParseDashDID).
//
// BIP-322 is intentionally not implemented — Dash wallets that produce
// signatures use BIP-137 exclusively. If a BIP-322 use case emerges,
// add it as a fallback path mirroring btc.go's verifyBIP322Simple.
func (d DashDID) Verify(data blocks.Block, sig string) (bool, error) {
	addr := d.Identifier()

	params := dashMainNetParams()
	if d.IsTestnet() {
		params = dashTestNetParams()
	}

	addrType, err := classifyDashAddress(addr, params)
	if err != nil {
		return false, fmt.Errorf("unsupported address: %w", err)
	}

	sigBytes, err := base64.StdEncoding.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %w", err)
	}

	if len(sigBytes) != 65 {
		return false, fmt.Errorf("invalid signature length %d for address type %s: expected 65 (BIP-137)", len(sigBytes), addrType)
	}

	return verifyDashBIP137(addr, addrType, params, data, sigBytes)
}

// ===== BIP-137 verification (Dash variant) =====

// verifyDashBIP137 recovers the pubkey from the compact signature and
// checks that the address derived from it matches the claimed DID address.
//
// Algorithm identical to btc.go's verifyBIP137 except for the message
// magic prefix and the chain params used for address derivation.
func verifyDashBIP137(addr string, addrType string, params *chaincfg.Params, data blocks.Block, sigBytes []byte) (bool, error) {
	hash := DashMessageHash(data.Cid().String())

	pubKey, _, err := ecdsa.RecoverCompact(sigBytes, hash)
	if err != nil {
		return false, fmt.Errorf("failed to recover public key: %w", err)
	}

	switch addrType {
	case "p2pkh":
		// Try compressed key first (standard for new Dash wallets).
		compHash := btcutil.Hash160(pubKey.SerializeCompressed())
		compAddr, err := btcutil.NewAddressPubKeyHash(compHash, params)
		if err != nil {
			return false, err
		}
		if compAddr.String() == addr {
			return true, nil
		}
		// Try uncompressed key (legacy P2PKH supports both forms).
		uncompHash := btcutil.Hash160(pubKey.SerializeUncompressed())
		uncompAddr, err := btcutil.NewAddressPubKeyHash(uncompHash, params)
		if err != nil {
			return false, err
		}
		return uncompAddr.String() == addr, nil

	case "p2sh":
		// P2SH-P2WPKH redeem script is 0x00 0x14 || hash160(compressed-pubkey).
		pubKeyHash := btcutil.Hash160(pubKey.SerializeCompressed())
		redeemScript := append([]byte{0x00, 0x14}, pubKeyHash...)
		derived, err := btcutil.NewAddressScriptHash(redeemScript, params)
		if err != nil {
			return false, err
		}
		return derived.String() == addr, nil
	}

	return false, fmt.Errorf("unknown address type")
}

// ===== helpers =====

// DashMessageHash computes the double-SHA256 hash of a Dash Signed Message.
// Format: \x19"DarkCoin Signed Message:\n" + varint(len(msg)) + msg
//
// The 0x19 byte is the length of "DarkCoin Signed Message:\n" (25 chars).
// This mirrors Bitcoin's "\x18" + "Bitcoin Signed Message:\n" exactly,
// just with the Dash magic prefix.
func DashMessageHash(msg string) []byte {
	prefix := []byte("\x19" + DashSignedMessageMagic)
	vi := writeDashVarInt(len(msg))
	buf := make([]byte, 0, len(prefix)+len(vi)+len(msg))
	buf = append(buf, prefix...)
	buf = append(buf, vi...)
	buf = append(buf, []byte(msg)...)
	first := sha256.Sum256(buf)
	second := sha256.Sum256(first[:])
	return second[:]
}

// writeDashVarInt encodes a non-negative integer using Bitcoin's variable-
// length integer format. Identical to btc.go's writeVarInt — duplicated
// here only to keep dash.go self-contained for audit purposes.
func writeDashVarInt(n int) []byte {
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

// classifyDashAddress decodes addr under the given params and returns a
// short type label suitable for the verifier switch.
func classifyDashAddress(addr string, params *chaincfg.Params) (string, error) {
	decoded, err := btcutil.DecodeAddress(addr, params)
	if err != nil {
		return "", err
	}
	switch decoded.(type) {
	case *btcutil.AddressPubKeyHash:
		return "p2pkh", nil
	case *btcutil.AddressScriptHash:
		return "p2sh", nil
	default:
		return "", fmt.Errorf("unsupported Dash address type: %T", decoded)
	}
}
