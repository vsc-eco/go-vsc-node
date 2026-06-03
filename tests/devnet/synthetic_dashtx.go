package devnet

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/bech32"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
)

// BuildSyntheticDashTxToAddress constructs a Bitcoin-wire-format raw
// transaction with:
//   - One synthetic P2PKH input: a random secp256k1 keypair + a
//     minimal valid DER-encoded signature. The signature doesn't
//     verify against any UTXO (the prev_hash is null) but
//     txscript.ComputePkScript can still recognise the
//     scriptSig as <sig> <pubkey> and derive the P2PKH pkScript →
//     ExtractPkScriptAddrs returns the Dash address. This satisfies
//     the dash-mapping-contract's ResolveSenderDashDID without
//     needing a real on-chain spend.
//   - One output of `amountSats` paying to `addr` via the
//     standard P2WSH script
//     (OP_0 <32-byte witness program>). The contract's
//     FindOutputAmount uses txscript.ExtractPkScriptAddrs on the
//     PkScript with `netParams` to recognise the address → matches
//     against the contract's re-derived deposit address.
//
// Returns the rawTxHex and the synthetic sender's Dash P2PKH address
// (so the test can correlate the contract-side sender resolution).
//
// IMPORTANT — this raw tx is NOT spendable on any real Dash chain.
// It's a contract-acceptance fixture: bytes that
// `wire.MsgTx.Deserialize` parses + that the contract's address
// matching logic accepts. The signature is DER-shaped but not a
// real ECDSA proof, the input outpoint is a null hash, etc.
//
// Used by the IS-login devnet E2E to drive the contract's
// mapInstantSendV2 success path without an actual Dash regtest
// payment — which is infeasible anyway because Dash never activated
// SegWit and dashd v23 rejects `tdash1...` P2WSH addresses at
// `sendtoaddress` time.
// DecodeDashP2WSH decodes a Dash-flavoured bech32 P2WSH address
// (`tdash1...` / `dash1...`) into its witness program. btcutil's
// DecodeAddress rejects unregistered HRPs ("decoded address is of
// unknown format"), and registering a forked Dash chaincfg has
// side effects across the global registry, so we strip the HRP +
// 5-bit decode manually and rebuild via NewAddressWitnessScriptHash
// against the harness-local params (which DON'T need to be
// registered for NewAddressWitnessScriptHash to work).
func DecodeDashP2WSH(addr string, netParams *chaincfg.Params) (*btcutil.AddressWitnessScriptHash, error) {
	_, decoded, err := bech32.Decode(addr)
	if err != nil {
		return nil, fmt.Errorf("bech32 decode: %w", err)
	}
	if len(decoded) < 1 {
		return nil, fmt.Errorf("empty bech32 program")
	}
	version := decoded[0]
	if version != 0 {
		return nil, fmt.Errorf("unsupported witness version %d (expected 0 for P2WSH)", version)
	}
	// Convert the 5-bit groups to 8-bit bytes.
	prog, err := bech32.ConvertBits(decoded[1:], 5, 8, false)
	if err != nil {
		return nil, fmt.Errorf("bech32 5→8 conversion: %w", err)
	}
	if len(prog) != 32 {
		return nil, fmt.Errorf("P2WSH program must be 32 bytes, got %d", len(prog))
	}
	return btcutil.NewAddressWitnessScriptHash(prog, netParams)
}

func BuildSyntheticDashTxToAddress(
	addr btcutil.Address,
	amountSats int64,
	netParams *chaincfg.Params,
) (rawTxHex string, senderAddr string, err error) {
	// 1. Generate the synthetic spender's keypair.
	var privBytes [32]byte
	if _, err := rand.Read(privBytes[:]); err != nil {
		return "", "", fmt.Errorf("priv rand: %w", err)
	}
	secpPriv := secp256k1.PrivKeyFromBytes(privBytes[:])
	pubBytes := secpPriv.PubKey().SerializeCompressed()

	// 2. Build a minimal DER-shaped fake signature. Format:
	//    0x30 <len> 0x02 <rLen> <rBytes> 0x02 <sLen> <sBytes> 0x01
	//    (trailing 0x01 = SIGHASH_ALL flag the wallet appends).
	//    btcd's ComputePkScript doesn't care that the sig is fake;
	//    it just walks the scriptSig push-by-push and reaches the
	//    pubkey at the end.
	sigDER := buildFakeDERSig()

	// 3. scriptSig = <sig> <pubkey>. txscript.ComputePkScript
	//    recognises this 2-push pattern as P2PKH-spend shape and
	//    produces the matching pkScript (HASH160(pubkey)).
	scriptSig, err := txscript.NewScriptBuilder().
		AddData(sigDER).
		AddData(pubBytes).
		Script()
	if err != nil {
		return "", "", fmt.Errorf("build scriptSig: %w", err)
	}

	// 4. PkScript for the output paying `addr`.
	pkScript, err := txscript.PayToAddrScript(addr)
	if err != nil {
		return "", "", fmt.Errorf("PayToAddrScript: %w", err)
	}

	// 5. Compose the tx.
	tx := wire.NewMsgTx(wire.TxVersion)
	tx.AddTxIn(&wire.TxIn{
		PreviousOutPoint: wire.OutPoint{
			Hash:  chainhash.Hash{}, // null prev
			Index: 0,
		},
		SignatureScript: scriptSig,
		Sequence:        0xffffffff,
	})
	tx.AddTxOut(&wire.TxOut{
		Value:    amountSats,
		PkScript: pkScript,
	})

	var buf bytes.Buffer
	if err := tx.Serialize(&buf); err != nil {
		return "", "", fmt.Errorf("tx serialize: %w", err)
	}
	rawTxHex = hex.EncodeToString(buf.Bytes())

	// 6. Derive the sender's P2PKH address for caller-side echoing.
	pubKeyHash := btcutil.Hash160(pubBytes)
	addrPKH, err := btcutil.NewAddressPubKeyHash(pubKeyHash, netParams)
	if err != nil {
		return "", "", fmt.Errorf("derive sender P2PKH: %w", err)
	}
	senderAddr = addrPKH.EncodeAddress()

	return rawTxHex, senderAddr, nil
}

// dashTestNetParamsForHarness mirrors cmd/is-service/deposit_address.go's
// dashTestNetParams. Duplicated here so the harness doesn't import
// the unexported function from the IS service package. The IS
// service's NewServer for -network=devnet inherits these via the
// "devnet" case in handlers.go.
func dashTestNetParamsForHarness() *chaincfg.Params {
	p := chaincfg.TestNet3Params
	p.PubKeyHashAddrID = 0x8c // 'y' prefix
	p.ScriptHashAddrID = 0x13 // '8'/'9' prefix
	p.Bech32HRPSegwit = "tdash"
	return &p
}

// buildFakeDERSig returns a 71-byte DER-shaped signature with a
// trailing 0x01 SIGHASH_ALL byte. The R and S are 32 bytes of zeros
// padded to 33 with a leading 0x00 (so the top bit isn't set, which
// would make DER reject as a "negative integer"). The signature
// doesn't verify against anything — it's structural padding for
// btcd's scriptSig parser.
func buildFakeDERSig() []byte {
	// 0x02 <32-byte r prepended with 0x00>  → 34 bytes (header + content)
	// 0x02 <32-byte s prepended with 0x00>  → 34 bytes
	// 0x30 + 1-byte length (68) + 68 bytes of content = 70 bytes
	// + 1 trailing SIGHASH_ALL = 71 bytes total.
	rs := make([]byte, 33)
	rs[0] = 0x01 // non-zero leading byte so the int isn't all zero
	sig := make([]byte, 0, 72)
	sig = append(sig, 0x30, 70) // SEQUENCE, 70 bytes follow
	sig = append(sig, 0x02, 33) // INTEGER, 33 bytes follow
	sig = append(sig, rs...)
	sig = append(sig, 0x02, 33) // INTEGER, 33 bytes follow
	sig = append(sig, rs...)
	sig = append(sig, 0x01) // SIGHASH_ALL
	return sig
}
