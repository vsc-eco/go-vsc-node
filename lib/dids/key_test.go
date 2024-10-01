package dids_test

import (
	"crypto/ed25519"
	"crypto/rand"
	"reflect"
	"strings"
	"testing"
	"vsc-node/lib/dids"

	"github.com/stretchr/testify/assert"
)

func TestSignVerify(t *testing.T) {
	// gen an ed25519 keypair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create a provider
	provider := dids.NewKeyProvider(privKey)

	// create a payload with consistent data types
	payload := map[string]interface{}{
		"someKey":  "someVal",
		"otherKey": float64(123),
	}

	// sign the payload
	signedJWT, err := provider.Sign(payload)
	assert.NoError(t, err)

	// create a DID from the pub key
	did, err := dids.NewKeyDID(pubKey)
	assert.NoError(t, err)

	// verify the signedJWT (JWS)
	valid, err := did.Verify(payload, signedJWT)
	assert.Nil(t, err)
	assert.True(t, valid)
}

func TestCreateDecryptJWE(t *testing.T) {
	// gen an ed25519 keypair for recipient
	recipientPubKey, recipientPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// gen an ed25519 keypair for sender
	_, senderPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create sender provider
	senderProvider := dids.NewKeyProvider(senderPrivKey)

	// create a payload
	payload := map[string]interface{}{
		"someKey":  "someVal",
		"otherKey": float64(123),
	}

	// encrypt the payload using the recipient's pub key
	jwe, err := senderProvider.CreateJWE(payload, recipientPubKey)
	assert.NoError(t, err)

	// create a recipient provider
	recipientProvider := dids.NewKeyProvider(recipientPrivKey)

	// decrypt the JWE using the recipient's priv key
	decryptedPayload, err := recipientProvider.DecryptJWE(jwe)
	assert.NoError(t, err)

	// ensure the decrypted payload matches the original
	assert.True(t, reflect.DeepEqual(payload, decryptedPayload))
}

func TestInvalidSignature(t *testing.T) {
	// gen an ed25519 keypair
	_, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create a provider
	provider := dids.NewKeyProvider(privKey)

	// create a payload
	payload := map[string]interface{}{
		"someKey":  "someVal",
		"otherKey": float64(123),
	}

	// sign the payload
	signedJWT, err := provider.Sign(payload)
	assert.NoError(t, err)

	// modify/tamper with the payload
	payload["otherKey"] = 999

	// create a DID from the public key
	did, err := dids.NewKeyDID(privKey.Public().(ed25519.PublicKey))
	assert.NoError(t, err)

	// parse the sigs from the signed JWT
	parts := strings.Split(signedJWT, ".")
	// SIGNED JWS must have 3 parts (JWE is another story)
	assert.Equal(t, 3, len(parts))
	sig := parts[2]

	// verify the tampered payload, it should fail or error out
	valid, err := did.Verify(payload, sig)
	assert.True(t, !valid || err != nil)
}

func TestDecryptJWEWithWrongKey(t *testing.T) {
	// gen an ed25519 keypair for the intended recipient
	recipientPubKey, _, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// gen an ed25519 keypair for another (wrong) recipient
	_, wrongPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// gen an ed25519 keypair for sender
	_, senderPrivKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create a sender provider
	senderProvider := dids.NewKeyProvider(senderPrivKey)

	// create a payload
	payload := map[string]interface{}{
		"someKey":  "someVal",
		"otherKey": float64(123),
	}

	// encrypt the payload using the intended recipient's pub key
	jwe, err := senderProvider.CreateJWE(payload, recipientPubKey)
	assert.NoError(t, err)

	// create a provider for the WRONG recipient
	wrongProvider := dids.NewKeyProvider(wrongPrivKey)

	// attempt to decrypt the JWE using the wrong recipient's priv key
	_, err = wrongProvider.DecryptJWE(jwe)
	// since the wrong key is used, an error should be thrown
	assert.Error(t, err)
}

func TestRecoverSigner(t *testing.T) {
	// gen an ed25519 keypair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create a provider
	provider := dids.NewKeyProvider(privKey)

	// create a payload
	payload := map[string]interface{}{
		"someKey":  "someVal",
		"otherKey": float64(123),
	}

	// sign the payload
	signedJWT, err := provider.Sign(payload)
	assert.NoError(t, err)

	// create a DID from the pub key
	did, err := dids.NewKeyDID(pubKey)
	assert.NoError(t, err)

	// use the RecoverSigner function to recover the signer DID from the signature
	recoveredDID, err := did.RecoverSigner(payload, signedJWT)
	assert.NoError(t, err)

	// assert that the recovered DID's identifier matches the original DID's identifier
	assert.Equal(t, did.Identifier(), recoveredDID.Identifier())

	// also assert that the DID matches what we expect from the original public key
	expectedDID, err := dids.NewKeyDID(pubKey)
	assert.NoError(t, err)
	assert.Equal(t, expectedDID, recoveredDID)
}

func TestRecoverSignerInvalidSignature(t *testing.T) {
	// gen an ed25519 keypair
	pubKey, privKey, err := ed25519.GenerateKey(rand.Reader)
	assert.NoError(t, err)

	// create a provider
	provider := dids.NewKeyProvider(privKey)

	// create a payload
	payload := map[string]interface{}{
		"someKey":  "someVal",
		"otherKey": float64(123),
	}

	// sign the payload
	signedJWT, err := provider.Sign(payload)
	assert.NoError(t, err)

	// create a DID from the pub key
	did, err := dids.NewKeyDID(pubKey)
	assert.NoError(t, err)

	// tamper with the sig by changing the sig part
	parts := strings.Split(signedJWT, ".")
	assert.Equal(t, 3, len(parts))
	tamperedJWT := parts[0] + "." + parts[1] + "." + "bad sig abc 123"

	// try to recover the signer using the tampered sig, which should fail
	_, err = did.RecoverSigner(payload, tamperedJWT)
	assert.Error(t, err)
}
