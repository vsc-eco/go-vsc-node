package dids_test

import (
	"crypto/ecdsa"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/stretchr/testify/assert"
)

var mockTypedData = apitypes.TypedData{
	Types: apitypes.Types{
		"EIP712Domain": []apitypes.Type{
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
		},
		"Message": []apitypes.Type{
			{Name: "message", Type: "string"},
		},
	},
	PrimaryType: "Message",
	Domain: apitypes.TypedDataDomain{
		Name:    "Hello world",
		Version: "1",
	},
	Message: apitypes.TypedDataMessage{
		"message": "Hello World",
	},
}

func TestNewEthProvider(t *testing.T) {
	// gen a new ECDSA priv key
	privKey, err := ecdsa.GenerateKey(crypto.S256(), rand.Reader)
	assert.NoError(t, err)

	// create a new eth provider
	provider, err := dids.NewEthProvider(privKey)
	assert.NoError(t, err)

	// ensure DID is not empty
	assert.NotEmpty(t, provider.DID().FullString())
}

func TestEthProviderSignAndVerifyTypedData(t *testing.T) {
	// gen a new ECDSA private key and create an eth provider
	privKey, err := ecdsa.GenerateKey(crypto.S256(), rand.Reader)
	assert.NoError(t, err)
	provider, err := dids.NewEthProvider(privKey)
	assert.NoError(t, err)

	// ref mock data
	typedData := mockTypedData

	// sign the typed data
	signature, err := provider.Sign(typedData)
	assert.NoError(t, err)

	// decode the signature from hex
	sigBytes, err := hex.DecodeString(signature)
	assert.NoError(t, err)
	assert.Equal(t, len(sigBytes), 65)

	// ensure V value is in [27, 28]
	if sigBytes[64] < 27 {
		sigBytes[64] += 27
	}

	// verify the sig
	valid, err := provider.VerifyTypedData(typedData, signature)
	assert.NoError(t, err)
	assert.True(t, valid)
}

func TestEthProviderRecoverSigner(t *testing.T) {
	// gen a new ECDSA priv key and create an eth provider
	privKey, err := crypto.GenerateKey()
	assert.NoError(t, err)
	provider, err := dids.NewEthProvider(privKey)
	assert.NoError(t, err)

	// create simple typed data
	typedData := mockTypedData

	// sign the typed data
	signature, err := provider.Sign(typedData)
	assert.NoError(t, err)

	// recover the signer
	recoveredDID, err := provider.RecoverSigner(typedData, signature)
	assert.NoError(t, err)
	assert.Equal(t, provider.DID().FullString(), recoveredDID.FullString())
}

func TestEthDIDAbidesByInterfaces(t *testing.T) {
	// gens a new ECDSA private key
	privKey, err := ecdsa.GenerateKey(crypto.S256(), rand.Reader)
	assert.NoError(t, err)

	// creates a new eth provider
	provider, err := dids.NewEthProvider(privKey)
	assert.NoError(t, err)

	// ensure it abides by both interfaces
	var _ dids.Provider = provider
	var _ dids.EthDIDProvider = provider
}
