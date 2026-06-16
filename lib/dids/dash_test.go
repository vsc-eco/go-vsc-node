package dids_test

import (
	"encoding/base64"
	"strings"
	"testing"
	"vsc-node/lib/dids"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/ecdsa"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/chaincfg"
	blocks "github.com/ipfs/go-block-format"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----- Dash chain params (mirrored from lib/dids/dash.go unexported helpers) -----
//
// We duplicate the params here so tests can construct addresses with the
// same encoding the production code uses. If dash.go's params drift,
// these tests will fail loudly on address mismatch.

func dashMainNetTestParams() *chaincfg.Params {
	p := chaincfg.MainNetParams
	p.PubKeyHashAddrID = 0x4c
	p.ScriptHashAddrID = 0x10
	p.Bech32HRPSegwit = "dash"
	return &p
}

func dashTestNetTestParams() *chaincfg.Params {
	p := chaincfg.TestNet3Params
	p.PubKeyHashAddrID = 0x8c
	p.ScriptHashAddrID = 0x13
	p.Bech32HRPSegwit = "tdash"
	return &p
}

// ----- helpers -----

// signDashBIP137 produces a base64-encoded BIP-137 compact signature using
// Dash's "DarkCoin Signed Message:\n" magic prefix.
func signDashBIP137(privKey *btcec.PrivateKey, msg string, compressed bool) string {
	hash := dids.DashMessageHash(msg)
	sig := ecdsa.SignCompact(privKey, hash, compressed)
	return base64.StdEncoding.EncodeToString(sig)
}

// ----- round-trip tests on each supported address type, mainnet -----

func TestDashDID_P2PKH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)
	// Sanity: Dash mainnet P2PKH addresses begin with 'X'.
	assert.True(t, strings.HasPrefix(addr.String(), "X"), "mainnet P2PKH should start with X, got %q", addr.String())

	did := dids.NewDashDID(addr.String())
	block := blocks.NewBlock([]byte("test dash p2pkh"))

	sig := signDashBIP137(privKey, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid)
}

func TestDashDID_P2SH_P2WPKH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	redeemScript := append([]byte{0x00, 0x14}, pubKeyHash...)
	addr, err := btcutil.NewAddressScriptHash(redeemScript, dashMainNetTestParams())
	require.NoError(t, err)
	// Sanity: Dash mainnet P2SH addresses begin with '7'.
	assert.True(t, strings.HasPrefix(addr.String(), "7"), "mainnet P2SH should start with 7, got %q", addr.String())

	did := dids.NewDashDID(addr.String())
	block := blocks.NewBlock([]byte("test dash p2sh"))

	sig := signDashBIP137(privKey, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid)
}

// Native segwit (P2WPKH) is intentionally not supported — see dash.go's
// validateDashAddress docstring. Real Dash wallets don't use bech32, and
// supporting it via btcutil would require global HRP registration.

// ----- testnet round-trips (proves we avoided btc.go's hardcoded-MainNetParams bug) -----

func TestDashDID_Testnet_P2PKH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashTestNetTestParams())
	require.NoError(t, err)
	// Sanity: Dash testnet P2PKH addresses begin with 'y'.
	assert.True(t, strings.HasPrefix(addr.String(), "y"), "testnet P2PKH should start with y, got %q", addr.String())

	did := dids.NewDashTestnetDID(addr.String())
	assert.True(t, did.IsTestnet())

	block := blocks.NewBlock([]byte("test dash testnet p2pkh"))
	sig := signDashBIP137(privKey, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid, "testnet DashDID must verify with testnet params (regression: btc.go had hardcoded MainNetParams)")
}

func TestDashDID_Testnet_P2SH(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	redeemScript := append([]byte{0x00, 0x14}, pubKeyHash...)
	addr, err := btcutil.NewAddressScriptHash(redeemScript, dashTestNetTestParams())
	require.NoError(t, err)
	// Sanity: Dash testnet P2SH addresses begin with '8' or '9'.
	first := addr.String()[:1]
	assert.True(t, first == "8" || first == "9", "testnet P2SH should start with 8 or 9, got %q", addr.String())

	did := dids.NewDashTestnetDID(addr.String())
	block := blocks.NewBlock([]byte("test dash testnet p2sh"))
	sig := signDashBIP137(privKey, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.True(t, valid)
}

// ----- parse/validation tests -----

func TestDashDID_RejectInvalidPrefix(t *testing.T) {
	// Eth DID
	_, err := dids.ParseDashDID("did:pkh:eip155:1:0x553Cb1F25f4409360E081E5e015812d1FB238e23")
	assert.Error(t, err)

	// Bitcoin DID (different bip122 chain id)
	_, err = dids.ParseDashDID("did:pkh:bip122:000000000019d6689c085ae165831e93:1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa")
	assert.Error(t, err)
}

func TestDashDID_RejectMalformedAddress(t *testing.T) {
	_, err := dids.ParseDashDID(dids.DashDIDPrefix + "not-a-real-address")
	assert.Error(t, err)
}

func TestDashDID_RejectWrongNetworkAddress(t *testing.T) {
	// A valid-looking testnet 'y...' address under mainnet prefix should reject.
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	testnetAddr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashTestNetTestParams())
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(testnetAddr.String(), "y"))

	// Attempt to parse as a mainnet DashDID — should fail because the version
	// byte doesn't match mainnet params.
	_, err = dids.ParseDashDID(dids.DashDIDPrefix + testnetAddr.String())
	assert.Error(t, err)
}

func TestDashDID_WrongKey(t *testing.T) {
	key1, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	key2, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	// Use a P2PKH address for key2 (we don't support P2WPKH for Dash).
	pubKeyHash2 := btcutil.Hash160(key2.PubKey().SerializeCompressed())
	addr2, err := btcutil.NewAddressPubKeyHash(pubKeyHash2, dashMainNetTestParams())
	require.NoError(t, err)

	did := dids.NewDashDID(addr2.String())
	block := blocks.NewBlock([]byte("dash spoof test"))

	// Sign with key1 but claim key2's DID — must NOT verify.
	sig := signDashBIP137(key1, block.Cid().String(), true)

	valid, err := did.Verify(block, sig)
	assert.NoError(t, err)
	assert.False(t, valid)
}

func TestDashDID_WrongMessage(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)

	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)

	did := dids.NewDashDID(addr.String())
	block1 := blocks.NewBlock([]byte("dash message one"))
	block2 := blocks.NewBlock([]byte("dash message two"))

	// Sign block1's CID but verify against block2.
	sig := signDashBIP137(privKey, block1.Cid().String(), true)

	valid, err := did.Verify(block2, sig)
	assert.NoError(t, err)
	assert.False(t, valid)
}

// ----- malformed signature handling -----

func TestDashDID_MalformedBase64(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)
	did := dids.NewDashDID(addr.String())
	block := blocks.NewBlock([]byte("malformed base64 test"))

	_, err = did.Verify(block, "not!valid!base64!!!")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "decode signature")
}

func TestDashDID_WrongSignatureLength(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)
	did := dids.NewDashDID(addr.String())
	block := blocks.NewBlock([]byte("wrong length test"))

	// 32 bytes instead of 65
	shortSig := base64.StdEncoding.EncodeToString(make([]byte, 32))
	_, err = did.Verify(block, shortSig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid signature length")

	// 66 bytes instead of 65
	longSig := base64.StdEncoding.EncodeToString(make([]byte, 66))
	_, err = did.Verify(block, longSig)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "invalid signature length")
}

func TestDashDID_EmptySignature(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)
	did := dids.NewDashDID(addr.String())
	block := blocks.NewBlock([]byte("empty sig test"))

	_, err = did.Verify(block, "")
	assert.Error(t, err)
}

// ----- identifier and roundtrip -----

func TestDashDID_IdentifierRoundtrip(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)
	addrStr := addr.String()

	did := dids.NewDashDID(addrStr)

	assert.Equal(t, addrStr, did.Identifier())
	assert.True(t, strings.HasPrefix(did.String(), dids.DashDIDPrefix))
	assert.True(t, strings.HasSuffix(did.String(), addrStr))
	assert.False(t, did.IsTestnet())

	parsed, err := dids.ParseDashDID(did.String())
	assert.NoError(t, err)
	assert.Equal(t, did.String(), parsed.String())
	assert.Equal(t, addrStr, parsed.Identifier())
}

func TestDashDID_Testnet_IdentifierRoundtrip(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashTestNetTestParams())
	require.NoError(t, err)
	addrStr := addr.String()

	did := dids.NewDashTestnetDID(addrStr)

	assert.Equal(t, addrStr, did.Identifier())
	assert.True(t, strings.HasPrefix(did.String(), dids.DashTestnetDIDPrefix))
	assert.True(t, did.IsTestnet())

	parsed, err := dids.ParseDashTestnetDID(did.String())
	assert.NoError(t, err)
	assert.Equal(t, did.String(), parsed.String())
	assert.Equal(t, addrStr, parsed.Identifier())
}

// ----- domain prefix sanity (for the future BLS attestation work) -----

func TestDashDID_DomainPrefixConstant(t *testing.T) {
	// The IS-lock attestation domain prefix must be a fixed NUL-terminated
	// string per the scoping spike decision memo. Any change here is a
	// breaking-protocol change requiring version bump.
	assert.Equal(t, "dash-is-lock-v1\x00", dids.DashISLockDomainPrefix)
}

// ----- message hash regression (catch accidental magic-prefix changes) -----

func TestDashMessageHash_DiffersFromBitcoin(t *testing.T) {
	// Dash signed-message digest MUST differ from Bitcoin's for the same
	// input string. If this ever passes-as-equal we know someone swapped
	// in the wrong magic prefix.
	msg := "QmTestCidThatNeverExists"
	dashHash := dids.DashMessageHash(msg)
	btcHash := dids.BitcoinMessageHash(msg)
	assert.NotEqual(t, dashHash, btcHash, "Dash and Bitcoin message hashes must differ — wrong magic prefix?")
}

// ----- Parse / VerifyAddress registration -----

func TestParse_DispatchesDashMainnet(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)
	didStr := dids.DashDIDPrefix + addr.String()

	parsed, err := dids.Parse(didStr)
	require.NoError(t, err)
	require.NotNil(t, parsed)
	assert.Equal(t, didStr, parsed.String())
}

func TestParse_DispatchesDashTestnet(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashTestNetTestParams())
	require.NoError(t, err)
	didStr := dids.DashTestnetDIDPrefix + addr.String()

	parsed, err := dids.Parse(didStr)
	require.NoError(t, err)
	require.NotNil(t, parsed)
	assert.Equal(t, didStr, parsed.String())
}

func TestVerifyAddress_DashMainnet(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashMainNetTestParams())
	require.NoError(t, err)
	didStr := dids.DashDIDPrefix + addr.String()

	assert.Equal(t, "user:dash", dids.VerifyAddress(didStr, true))
	assert.Equal(t, "unknown", dids.VerifyAddress(didStr, false), "mainnet DID must not be accepted on testnet")
}

func TestVerifyAddress_DashTestnet(t *testing.T) {
	privKey, err := btcec.NewPrivateKey()
	require.NoError(t, err)
	pubKeyHash := btcutil.Hash160(privKey.PubKey().SerializeCompressed())
	addr, err := btcutil.NewAddressPubKeyHash(pubKeyHash, dashTestNetTestParams())
	require.NoError(t, err)
	didStr := dids.DashTestnetDIDPrefix + addr.String()

	assert.Equal(t, "user:dash", dids.VerifyAddress(didStr, false))
	assert.Equal(t, "unknown", dids.VerifyAddress(didStr, true), "testnet DID must not be accepted on mainnet")
}
