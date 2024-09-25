package did_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"vsc-node/lib/did"

	"github.com/stretchr/testify/assert"
)

func TestNewEd25519Provider(t *testing.T) {
	// seed must be 32 bytes
	seed := [32]byte{}
	copy(seed[:], "anAbsoluteUnitOfAHumongousFoobar")
	// use the random high-entropy seed to gen a new ed25519 key pair (32-bytes)
	_, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)
}

func TestSettingGettingDID(t *testing.T) {
	// create provider
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)

	// get pub key from DID... does it match the provider's pub key?
	pubKeyFromDID, err := provider.DID().PublicKey()
	assert.Nil(t, err)
	assert.Equal(t, provider.PublicKey(), pubKeyFromDID)
}

func TestDIDValidity(t *testing.T) {
	// invalid
	invalidDID := did.DID("did:key:oopsie")
	assert.False(t, invalidDID.IsValid())

	// valid
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)
	assert.True(t, provider.DID().IsValid())
}

func TestCreateAndVerifyJWS(t *testing.T) {
	// create provider
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)

	// some payload that we want to sign
	payload := map[string]interface{}{
		"foo": "bar",
	}

	// create JWS
	jws, err := provider.CreateJWS(payload)
	assert.Nil(t, err)

	// verify JWS
	verifiedPayload, err := provider.VerifyJWS(jws)
	assert.Nil(t, err)

	// assert that the payload is the same
	assert.Equal(t, payload, verifiedPayload)

	// tampered JWS
	tamperedJWS := jws + "a"
	_, err = provider.VerifyJWS(tamperedJWS)
	assert.NotNil(t, err)
	assert.Equal(t, did.ErrInvalidSignature, err)
}

func TestCreateAndDecryptJWE(t *testing.T) {
	// create provider
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)

	// original payload
	originalPayload := map[string]interface{}{
		"foo": "bar",
	}

	// create JWE
	jwe, err := provider.CreateJWE(originalPayload, provider.DID())
	assert.Nil(t, err)
	t.Logf("JWE: %s", jwe)

	// decrypt JWE
	decryptedPayload, err := provider.DecryptJWE(jwe)
	assert.Nil(t, err)

	// assert that the payload is the same
	assert.Equal(t, originalPayload, decryptedPayload)

	// tamper with the JWE and ensure serialization/deserialization fails
	tamperedJWE := jwe + "a"
	_, err = provider.DecryptJWE(tamperedJWE)
	assert.NotNil(t, err)
	assert.True(t, errors.Is(err, did.ErrSerialization) || errors.Is(err, did.ErrDeserialization))
}

func TestInvalidDIDForJWE(t *testing.T) {
	// create provider
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)

	// original payload
	originalPayload := map[string]interface{}{
		"foo": "bar",
	}

	// invalid DID
	invalidDID := "did:key:invalid"

	// create JWE with invalid recipient DID
	_, err = provider.CreateJWE(originalPayload, did.DID(invalidDID))
	assert.NotNil(t, err)
	assert.Equal(t, did.ErrInvalidDID, err)
}

func TestTamperedPayloadJWE(t *testing.T) {
	// create provider
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)

	// original payload
	originalPayload := map[string]interface{}{
		"foo":   "bar",
		"hello": "world",
	}

	// create JWE
	jwe, err := provider.CreateJWE(originalPayload, provider.DID())
	assert.Nil(t, err)

	// tamper with the JWE
	jweMap := make(map[string]string)
	err = json.Unmarshal([]byte(jwe), &jweMap)
	assert.Nil(t, err)
	jweMap["cipher"] = base64.RawURLEncoding.EncodeToString([]byte("tampered"))

	// re-encode the tampered JWE
	tamperedJWE, err := json.Marshal(jweMap)
	assert.Nil(t, err)

	// try decrypting the tampered JWE
	_, err = provider.DecryptJWE(string(tamperedJWE))
	assert.Equal(t, did.ErrDecryptionFailed, err)
}

func TestInvalidSignatureJWS(t *testing.T) {
	// create provider
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)

	// dummy payload that we want to sign
	payload := map[string]interface{}{
		"foo": "bar",
	}

	// create JWS
	jws, err := provider.CreateJWS(payload)
	assert.Nil(t, err)

	// ensure it has 3 parts (in form "header.payload.sig")
	parts := strings.Split(jws, ".")
	assert.Len(t, parts, 3)

	// tamper with the sig to be invalid encoding ("|" is not valid url base64 encoding)
	sigWithDiffLastChar := parts[2][:len(parts[2])-1] + "|"
	tamperedJWS := parts[0] + "." + parts[1] + "." + sigWithDiffLastChar

	// try verifying badly encoded JWS sig
	_, err = provider.VerifyJWS(tamperedJWS)
	assert.Equal(t, did.ErrInvalidEncoding, err)

	// tamper with the sig to be just "tampered with, but still valid encoding"
	//
	// we add 2 X's just in case the actual last character was an X
	sigWithDiffLastChar = parts[2][:len(parts[2])-1] + "XX"
	tamperedJWS = parts[0] + "." + parts[1] + "." + sigWithDiffLastChar

	// try verifying badly encoded JWS sig
	_, err = provider.VerifyJWS(tamperedJWS)
	assert.Equal(t, did.ErrInvalidSignature, err)

}

func TestHugePayload(t *testing.T) {
	// create provider
	seed := [32]byte{}
	copy(seed[:], "asuperduperfoobarthatisthelength")
	provider, err := did.NewEd25519Provider(seed)
	assert.Nil(t, err)

	// create a very very very large payload
	payload := map[string]interface{}{}

	for _, i := range []int{1, 2, 3, 4, 5} {
		payload[fmt.Sprint(i)] = strings.Repeat("abc", 100000)
	}

	// create JWS
	jws, err := provider.CreateJWS(payload)
	assert.Nil(t, err)

	// create JWE
	jwe, err := provider.CreateJWE(payload, provider.DID())
	assert.Nil(t, err)

	// verify JWS
	verifiedPayload, err := provider.VerifyJWS(jws)
	assert.Nil(t, err)

	// assert that the payload is the same
	assert.Equal(t, payload, verifiedPayload)

	// decrypt JWE
	decryptedPayload, err := provider.DecryptJWE(jwe)
	assert.Nil(t, err)

	// assert that the payload is the same
	assert.Equal(t, payload, decryptedPayload)

	// tamper with the JWE and ensure decryption fails
	tamperedJWE := jwe + "tampertamper"
	_, err = provider.DecryptJWE(tamperedJWE)
	assert.NotNil(t, err)

	// tamper with the JWS and ensure verifying it fails
	tamperedJWS := jws + "tampertamper"
	_, err = provider.VerifyJWS(tamperedJWS)
	assert.NotNil(t, err)
}
