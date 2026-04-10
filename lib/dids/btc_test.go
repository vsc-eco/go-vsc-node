package dids_test

import (
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"vsc-node/lib/dids"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
	"github.com/btcsuite/btcd/wire"
	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func signBIP137(privKey *btcec.PrivateKey, msg string, compressed bool) string {
	hash := dids.BitcoinMessageHash(msg)
	sig := ecdsa.SignCompact(privKey, hash, compressed)
	return base64.StdEncoding.EncodeToString(sig)
}

func TestBtcDID_P2PKH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	assert.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	assert.NoError(t, err)

	did := dids.NewBtcDID(addr.String())
	block := blocks.NewBlock([]byte("test p2pkh"))

	sig := signBIP137(privKey, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid)
}

func TestBtcDID_P2SH_P2WPKH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	assert.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	redeemScript := append([]byte{0x00, 0x14}, pubKeyHash...)
	addr, err := btcutil.NewAddressScriptHash(redeemScript, &chaincfg.MainNetParams)
	assert.NoError(t, err)

	did := dids.NewBtcDID(addr.String())
	block := blocks.NewBlock([]byte("test p2sh"))

	sig := signBIP137(privKey, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid)
}

func TestBtcDID_P2WPKH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	assert.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	assert.NoError(t, err)

	did := dids.NewBtcDID(addr.String())
	block := blocks.NewBlock([]byte("test p2wpkh"))

	sig := signBIP137(privKey, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid)
}

func TestBtcDID_RejectTaproot(t *testing.T) {
	_, err := dids.ParseBtcDID(
		"did:pkh:bip122:000000000019d6689c085ae165831e93:bc1p5d7rjq7g6rdk2yhzks9smlaqtedr4dekq08ge8ztwac72sfr9rusxg3s7p",
	)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "taproot")
}

func TestBtcDID_RejectInvalidPrefix(t *testing.T) {
	_, err := dids.ParseBtcDID("did:pkh:eip155:1:0x553Cb1F25f4409360E081E5e015812d1FB238e23")
	assert.Error(t, err)
}

func TestBtcDID_WrongKey(t *testing.T) {
	// a signature from key1 cannot authenticate as key2's address DID
	key1, err := btcec.NewPrivateKey()
	assert.NoError(t, err)
	key2, err := btcec.NewPrivateKey()
	assert.NoError(t, err)

	// key2's P2WPKH address
	pubKeyHash2 := btcutil.Hash160(key2.PubKey().SerializeCompressed())
	addr2, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash2, &chaincfg.MainNetParams)
	assert.NoError(t, err)

	// DID claims key2's address
	did := dids.NewBtcDID(addr2.String())
	block := blocks.NewBlock([]byte("spoofing test"))

	// Sign with key1 (not key2)
	sig := signBIP137(key1, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.False(t, valid)
}

func TestBtcDID_MalformedBase64(t *testing.T) {
	did := dids.NewBtcDID("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	block := blocks.NewBlock([]byte("malformed base64 test"))

	_, err := did.Verify(block, "not!valid!base64!!!")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decode signature")
}

func TestBtcDID_WrongSignatureLength(t *testing.T) {
	did := dids.NewBtcDID("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	block := blocks.NewBlock([]byte("wrong length test"))

	// 32 bytes instead of 65
	shortSig := base64.StdEncoding.EncodeToString(make([]byte, 32))
	_, err := did.Verify(block, shortSig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid signature length")

	// 66 bytes instead of 65
	longSig := base64.StdEncoding.EncodeToString(make([]byte, 66))
	_, err = did.Verify(block, longSig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid signature length")
}

func TestBtcDID_EmptySignature(t *testing.T) {
	did := dids.NewBtcDID("1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	block := blocks.NewBlock([]byte("empty sig test"))

	_, err := did.Verify(block, "")
	assert.Error(t, err)
}

func TestBtcDID_IdentifierRoundtrip(t *testing.T) {
	addr := "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa"
	did := dids.NewBtcDID(addr)

	// Identifier() should return just the address
	assert.Equal(t, addr, did.Identifier())

	// String() should return the full DID
	assert.True(t, strings.HasPrefix(did.String(), "did:pkh:bip122:"))
	assert.True(t, strings.HasSuffix(did.String(), addr))

	// ParseBtcDID roundtrip
	parsed, err := dids.ParseBtcDID(did.String())
	assert.NoError(t, err)
	assert.Equal(t, did.String(), parsed.String())
	assert.Equal(t, addr, parsed.Identifier())
}

// signBIP322 creates a BIP-322 "simple" signature for a P2WPKH address.
// This mirrors what Leather wallet does for bc1q addresses.
func signBIP322(t *testing.T, privKey *btcec.PrivateKey, msg string) string {
	t.Helper()

	pubkey := privKey.PubKey()
	pubkeyHash := btcutil.Hash160(pubkey.SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubkeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)
	scriptPubKey, err := txscript.PayToAddrScript(addr)
	require.NoError(t, err)

	// BIP-322 tagged message hash
	tag := sha256.Sum256([]byte("BIP0322-signed-message"))
	h := sha256.New()
	h.Write(tag[:])
	h.Write(tag[:])
	h.Write([]byte(msg))
	msgHash := h.Sum(nil)

	// Build to_spend transaction
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

	// Build to_sign transaction
	toSpendHash := toSpend.TxHash()
	toSign := wire.NewMsgTx(0)
	toSignIn := wire.NewTxIn(wire.NewOutPoint(&toSpendHash, 0), nil, nil)
	toSignIn.Sequence = 0
	toSign.AddTxIn(toSignIn)
	toSign.AddTxOut(wire.NewTxOut(0, []byte{txscript.OP_RETURN}))

	// Compute BIP-143 sighash
	scriptCode, err := txscript.NewScriptBuilder().
		AddOp(txscript.OP_DUP).
		AddOp(txscript.OP_HASH160).
		AddData(pubkeyHash).
		AddOp(txscript.OP_EQUALVERIFY).
		AddOp(txscript.OP_CHECKSIG).
		Script()
	require.NoError(t, err)

	prevOutputFetcher := txscript.NewCannedPrevOutputFetcher(scriptPubKey, 0)
	sigHashes := txscript.NewTxSigHashes(toSign, prevOutputFetcher)
	sighashBytes, err := txscript.CalcWitnessSigHash(scriptCode, sigHashes, txscript.SigHashAll, toSign, 0, 0)
	require.NoError(t, err)

	// Sign with ECDSA
	sig := ecdsa.Sign(privKey, sighashBytes)
	derSig := sig.Serialize()
	sigWithSighash := append(derSig, byte(txscript.SigHashAll))

	// Serialize witness stack: [num_items, sig_len, sig, pubkey_len, pubkey]
	compressedPubkey := pubkey.SerializeCompressed()
	witness := make([]byte, 0, 1+1+len(sigWithSighash)+1+len(compressedPubkey))
	witness = append(witness, 2) // num items
	witness = append(witness, byte(len(sigWithSighash)))
	witness = append(witness, sigWithSighash...)
	witness = append(witness, byte(len(compressedPubkey)))
	witness = append(witness, compressedPubkey...)

	return base64.StdEncoding.EncodeToString(witness)
}

func TestBtcDID_BIP322_P2WPKH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := dids.NewBtcDID(addr.String())
	block := blocks.NewBlock([]byte("test bip322 p2wpkh"))

	sig := signBIP322(t, privKey, block.Cid().String())

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid, "BIP-322 P2WPKH signature must verify")
}

func TestBtcDID_BIP322_WrongKey(t *testing.T) {
	key1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// DID claims key2's address
	pubKeyHash2 := btcutil.Hash160(key2.PubKey().SerializeCompressed())
	addr2, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash2, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := dids.NewBtcDID(addr2.String())
	block := blocks.NewBlock([]byte("bip322 wrong key test"))

	// Sign with key1 (not key2) — signBIP322 uses key1's address internally,
	// but we'll verify against key2's DID, so pubkey won't match
	sig := signBIP322(t, key1, block.Cid().String())

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.False(t, valid, "BIP-322 signature from wrong key must not verify")
}

func TestBtcDID_BIP322_WrongMessage(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := dids.NewBtcDID(addr.String())
	block1 := blocks.NewBlock([]byte("message one"))
	block2 := blocks.NewBlock([]byte("message two"))

	// Sign block1's CID but verify against block2
	sig := signBIP322(t, privKey, block1.Cid().String())

	valid, err := did.Verify(block2, sig)
	assert.NoError(t, err)
	assert.False(t, valid, "BIP-322 signature for wrong message must not verify")
}

func TestBtcDID_P2WPKH_BothFormats(t *testing.T) {
	// Same key should verify with both BIP-137 and BIP-322 signatures
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressWitnessPubKeyHash(pubKeyHash, &chaincfg.MainNetParams)
	require.NoError(t, err)

	did := dids.NewBtcDID(addr.String())
	block := blocks.NewBlock([]byte("dual format test"))
	cidStr := block.Cid().String()

	// BIP-137 signature
	sig137 := signBIP137(privKey, cidStr, true)
	valid, err := did.Verify(block, sig137)
	assert.NoError(t, err)
	assert.True(t, valid, "BIP-137 must verify for P2WPKH")

	// BIP-322 signature
	sig322 := signBIP322(t, privKey, cidStr)
	valid, err = did.Verify(block, sig322)
	assert.NoError(t, err)
	assert.True(t, valid, "BIP-322 must verify for P2WPKH")
}

func TestBtcDID_RealWalletSignature(t *testing.T) {
	// Real signature from Bitcoin Core v28.1.0
	// Address: 15NsrHB1sNbVsWFcoK9BPA7qVWfA7eyN4d (P2PKH legacy)
	// Block content: []byte("test vsc btc did")
	// CID: QmbCB9UokqQzyHX1DW1gT95yMrCBX5YDL5oHmS7CswBSf8
	// Signature produced by: signmessage "15NsrHB1sNbVsWFcoK9BPA7qVWfA7eyN4d" "<CID>"
	addr := "15NsrHB1sNbVsWFcoK9BPA7qVWfA7eyN4d"
	sig := "IDu5SKlO49iXHnpdrH5CdVFqCtjuwjF97ZRkuZ+UB3Y7OCpOyHeUuqfTw2ZZ+paid8MucuMpk1FRjuv4kqvQ6JI="
	block := blocks.NewBlock([]byte("test vsc btc did"))

	did := dids.NewBtcDID(addr)
	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid, "real Bitcoin Core signature must verify correctly")
}
