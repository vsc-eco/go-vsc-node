package dids_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"vsc-node/lib/dids"

	"github.com/stretchr/testify/assert"
)

func TestNewKeyProvider(t *testing.T) {
	// gens a new ed25519 key pair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// creates a new key provider
	provider, err := dids.NewKeyProvider(privKey)
	assert.NoError(t, err)
	assert.NotNil(t, provider)

	// retrieves the DID
	did := provider.DID()
	assert.NotEmpty(t, did)

	// extract the pub key from the DID
	extractedPubKey, err := dids.PubKeyFromKeyDID(did.FullString())
	assert.NoError(t, err)

	// ensure the extracted pub key matches the original
	assert.EqualValues(t, pubKey, extractedPubKey)
}

func TestCreateAndVerifyJWS(t *testing.T) {
	// gen a new ed25519 key pair
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create a key provider
	provider, err := dids.NewKeyProvider(privKey)
	assert.NoError(t, err)
	assert.NotNil(t, provider)

	// dummy payload
	payload := map[string]interface{}{
		"hullo": "wurld",
	}

	// create JWS
	jws, err := provider.Sign(payload)
	assert.NoError(t, err)
	assert.NotEmpty(t, jws)

	// verify JWS
	provPubKey, err := dids.PubKeyFromKeyDID(provider.DID().FullString())
	assert.NoError(t, err)

	verifiedPayload, err := dids.VerifyJWS(provPubKey, jws)
	assert.NoError(t, err)
	assert.Equal(t, payload, verifiedPayload)
}

func TestCreateAndDecryptJWE(t *testing.T) {
	// gen key pairs for sender and sending to (recipient)
	_, senderPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)
	_, recipientPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create key pairs for sender and sending to
	senderProvider, err := dids.NewKeyProvider(senderPrivKey)
	assert.NoError(t, err)
	assert.NotNil(t, senderProvider)

	recipientProvider, err := dids.NewKeyProvider(recipientPrivKey)
	assert.NoError(t, err)
	assert.NotNil(t, recipientProvider)

	// create a dummy (secret) payload
	payload := map[string]interface{}{
		"shhhh": "dont tell",
	}

	//  reates a JWE for the recipient
	jwe, err := senderProvider.CreateJWE(payload, recipientProvider.DID())
	assert.NoError(t, err)
	assert.NotEmpty(t, jwe)

	// decrypt the JWE
	decryptedPayload, err := recipientProvider.DecryptJWE(jwe)
	assert.NoError(t, err)

	// decrypted payload should match the original
	assert.Equal(t, payload, decryptedPayload)
}

func TestInvalidDID(t *testing.T) {
	// invalid DID
	invalidDID := dids.KeyDID("bad-did")

	// attempt extracting the pub key
	_, err := dids.PubKeyFromKeyDID(invalidDID.FullString())
	assert.Error(t, err)
}

func TestInvalidSignature(t *testing.T) {
	// gen two different ed25519 key pairs
	_, privKey1, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)
	_, privKey2, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create two key providers
	provider1, err := dids.NewKeyProvider(privKey1)
	assert.NoError(t, err)
	provider2, err := dids.NewKeyProvider(privKey2)
	assert.NoError(t, err)

	// dummy payload
	payload := map[string]interface{}{
		"foo": "bar",
	}

	// prov 1 creates a JWS
	jws, err := provider1.Sign(payload)
	assert.NoError(t, err)
	assert.NotEmpty(t, jws)

	// prov 2 attempts to verify the JWS
	prov2PubKey, err := dids.PubKeyFromKeyDID(provider2.DID().FullString())
	assert.NoError(t, err)

	_, err = dids.VerifyJWS(prov2PubKey, jws)
	// should error out
	assert.Error(t, err)
}

func TestDecryptionFailure(t *testing.T) {
	// gen key pairs for sender and recipients
	_, senderPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)
	_, recipientPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)
	_, wrongRecipientPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create their key providers
	senderProvider, err := dids.NewKeyProvider(senderPrivKey)
	assert.NoError(t, err)
	recipientProvider, err := dids.NewKeyProvider(recipientPrivKey)
	assert.NoError(t, err)
	wrongRecipientProvider, err := dids.NewKeyProvider(wrongRecipientPrivKey)
	assert.NoError(t, err)

	// dummy payload
	payload := map[string]interface{}{
		"shhhh": "don't tell, it's secret",
	}

	// creates a JWE for its intended recipient
	jwe, err := senderProvider.CreateJWE(payload, recipientProvider.DID())
	assert.NoError(t, err)
	assert.NotEmpty(t, jwe)

	// wrong recipient attempts to decrypt the JWE, which should fail
	_, err = wrongRecipientProvider.DecryptJWE(jwe)
	assert.Error(t, err)
}

func TestKeyDIDAbidesByInterfaces(t *testing.T) {

	// gens a new ed25519 key pair
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// creates a key provider
	provider, err := dids.NewKeyProvider(privKey)
	assert.NoError(t, err)
	assert.NotNil(t, provider)

	var _ dids.KeyDIDProvider = provider
	var _ dids.Provider = provider

	did := provider.DID()
	var _ dids.DID = did
}
