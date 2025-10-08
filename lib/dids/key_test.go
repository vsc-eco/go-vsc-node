package dids_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"
)

func TestCreateKeyDID(t *testing.T) {
	pubKey, _, _ := ed25519.GenerateKey(rand.Reader)
	assert.NotNil(t, pubKey)

	did, err := dids.NewKeyDID(pubKey)
	assert.Nil(t, err)
	assert.NotNil(t, did)
}

func TestCreateKeyDIDProvider(t *testing.T) {
	_, privKey, _ := ed25519.GenerateKey(rand.Reader)
	assert.NotNil(t, privKey)

	provider := dids.NewKeyProvider(privKey)
	assert.NotNil(t, provider)
}

func TestCreateDecryptJWE(t *testing.T) {
	// gen a random payload for the test
	payload := map[string]interface{}{
		"name": "bob",
		"mark": 64.4,
	}

	// gen a key pair for the recipient
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.Nil(t, err)

	// encrypt the payload using the recipient's public key
	jwe, err := dids.CreateJWE(payload, pubKey)
	assert.Nil(t, err)

	// init a key provider using the recipient's priv key for decryption
	keyProvider := dids.NewKeyProvider(privKey)

	// decrypt the JWE using the recipient's priv key
	decryptedPayload, err := keyProvider.DecryptJWE(jwe)
	assert.Nil(t, err)

	// both should match
	assert.Equal(t, payload, decryptedPayload)
}

func TestInvalidKeyDIDCreation(t *testing.T) {
	var invalidPubKey ed25519.PublicKey
	_, err := dids.NewKeyDID(invalidPubKey)
	assert.NotNil(t, err)
}

func TestMalformedJWE(t *testing.T) {
	// create a malformed JWE
	malformedJWE := "bad..jwe"

	_, privKey, _ := ed25519.GenerateKey(rand.Reader)
	keyProvider := dids.NewKeyProvider(privKey)

	// expect decryption to fail due to malformed JWE
	_, err := keyProvider.DecryptJWE(malformedJWE)
	assert.NotNil(t, err)
}

func TestJWEDecryptionWithWrongKey(t *testing.T) {
	payload := map[string]interface{}{
		"name": "alice",
	}

	// gen two key pairs
	pubKey1, _, _ := ed25519.GenerateKey(rand.Reader)
	_, privKey2, _ := ed25519.GenerateKey(rand.Reader)

	// encrypt with pubKey1
	jwe, err := dids.CreateJWE(payload, pubKey1)
	assert.Nil(t, err)

	// try decrypting with privKey2
	keyProvider := dids.NewKeyProvider(privKey2)
	_, err = keyProvider.DecryptJWE(jwe)

	// this should fail, so non-nil
	assert.NotNil(t, err)
}

func TestBasicSignVerify(t *testing.T) {
	// gen a keypair
	pubKey, privKey, _ := ed25519.GenerateKey(rand.Reader)
	assert.NotNil(t, pubKey)

	provider := dids.NewKeyProvider(privKey)

	// create original block
	block := blocks.NewBlock([]byte("hello world"))
	assert.NotNil(t, block)

	jws1, err := provider.Sign(block)
	assert.Nil(t, err)

	// create DID from the pub key
	did, err := dids.NewKeyDID(pubKey)
	assert.Nil(t, err)

	// verify the original block with its sig
	valid, err := did.Verify(block, jws1)
	assert.Nil(t, err)
	assert.True(t, valid)

	// create modified/incorrect block with different content
	modifiedBlock := blocks.NewBlock([]byte("data doesn't matter"))
	valid, err = did.Verify(modifiedBlock, jws1)
	assert.NotNil(t, err)
	assert.False(t, valid)
}
func TestRealIpfsVerify(t *testing.T) {
	// parse the real provided CID into a dummy block with that set CID
	realBlock, err := blocks.NewBlockWithCid([]byte("data doesn't matter"), cid.MustParse("bafyreiapoy2k666lcpsnbbilyzloqbc64mevrkymorujyi4cjqkzp2ukj4"))
	assert.Nil(t, err)

	// the real DID from the transaction
	realKeyDid := "did:key:z6MkoqsapCm3eQXQBByJTFcL5SMFg7d3aH8iobtmoL8tt6yw"

	// the real sig from the transaction
	realSig := "SV1jI7m-Fs59cBioCxrV1oLIbsbA-nDAQtpFUFDvr2iAxeq5JnfUHb2N8z2N78Lt4PnsMeX9flaq-IpzN97wDA"

	// create the KeyDID from the realKeyDid
	keyDID := dids.KeyDID(realKeyDid)

	// call the Verify function with the full signature
	verified, err := keyDID.Verify(realBlock, realSig)
	assert.Nil(t, err)
	assert.True(t, verified)
}
