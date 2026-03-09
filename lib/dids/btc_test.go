package dids_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"vsc-node/lib/dids"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/btcutil"
	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/assert"
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
		"did:pkh:bip122:000000000019d6689c085ae165831e93/bc1p5d7rjq7g6rdk2yhzks9smlaqtedr4dekq08ge8ztwac72sfr9rusxg3s7p",
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
