package dids_test

import (
	"crypto/ecdsa"
	"encoding/hex"
	"testing"

	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/stretchr/testify/assert"
)

// mock typed data (abiding by EIP-712) for the mocks/tests
// akin to: https://github.com/vsc-eco/client/blob/483a3f7bc182a75785fb8e1118868bfe6e60db97/src/utils.ts#L96
// but uses the official github.com/ethereum/go-ethereum lib (don't reinvent the wheel)
var mockTypedData = apitypes.TypedData{
	Domain: apitypes.TypedDataDomain{
		Name:              "vsc app",
		Version:           "1",
		ChainId:           math.NewHexOrDecimal256(1),
		VerifyingContract: "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
	},
	PrimaryType: "vsc",
	Message: apitypes.TypedDataMessage{
		"from": apitypes.TypedDataMessage{
			"name":   "bob",
			"wallet": "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
		},
		"to": apitypes.TypedDataMessage{
			"name":   "alice",
			"wallet": "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
		},
		"contents": "hello world",
	},
	Types: apitypes.Types{
		"EIP712Domain": []apitypes.Type{
			{Name: "name", Type: "string"},
			{Name: "version", Type: "string"},
			{Name: "chainId", Type: "uint256"},
			{Name: "verifyingContract", Type: "address"},
		},
		"vsc": []apitypes.Type{
			{Name: "from", Type: "User"},
			{Name: "to", Type: "User"},
			{Name: "contents", Type: "string"},
		},
		"User": []apitypes.Type{
			{Name: "name", Type: "string"},
			{Name: "wallet", Type: "address"},
		},
	},
}

// gen a valid signature for the EIP-712 typed data using the provided priv key
//
// this is a helper function for the tests, and should not be used in production
func generateValidSignature(typedData apitypes.TypedData, privateKey *ecdsa.PrivateKey) (string, error) {
	// compute the EIP-712 hash
	dataHash, err := dids.ComputeEIP712Hash(typedData)
	if err != nil {
		return "", err
	}

	// sign the hash
	sig, err := crypto.Sign(dataHash, privateKey)
	if err != nil {
		return "", err
	}

	// encode the sig in hex format for eth tests
	return hex.EncodeToString(sig), nil
}

func TestEthDIDVerify(t *testing.T) {
	// gen a real priv key for signing
	privateKey, err := crypto.GenerateKey()
	assert.NoError(t, err)

	// gen a real sig for our mocked data type
	sigHex, err := generateValidSignature(mockTypedData, privateKey)
	assert.NoError(t, err)

	// get the addr from the priv key
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	ethDid := dids.NewEthDID(address)

	// verify the valid sig
	valid, err := ethDid.Verify(mockTypedData, sigHex)
	assert.NoError(t, err)
	// ensure the sig is valid, it should be
	assert.True(t, valid)
}

func TestEthDIDRecoverSigner(t *testing.T) {
	// gen a real priv key for signing
	privateKey, err := crypto.GenerateKey()
	assert.NoError(t, err)

	// gen a valid sig for our mocked data
	sigHex, err := generateValidSignature(mockTypedData, privateKey)
	assert.NoError(t, err)

	// get the expected addr from the priv key
	expectedAddress := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()

	// init the EthDID
	ethDid := dids.NewEthDID(expectedAddress)

	// recover the signer from the sig
	recoveredDID, err := ethDid.RecoverSigner(mockTypedData, sigHex)
	assert.NoError(t, err)

	// check that the recovered DID's identifier matches the expected address
	assert.Equal(t, expectedAddress, recoveredDID.Identifier())
}

func TestEthDIDVerifyInvalidSig(t *testing.T) {
	// gen a real priv key for signing
	privateKey, err := crypto.GenerateKey()
	assert.NoError(t, err)

	// gen a real sig for our mocked data type
	sigHex, err := generateValidSignature(mockTypedData, privateKey)
	assert.NoError(t, err)

	// get the addr from the priv key
	address := crypto.PubkeyToAddress(privateKey.PublicKey).Hex()
	ethDid := dids.NewEthDID(address)

	// verify the valid sig
	valid, err := ethDid.Verify(mockTypedData, sigHex)
	assert.NoError(t, err)
	// ensure the sig is valid, it should be
	assert.True(t, valid)

	// verify an invalid sig
	valid, err = ethDid.Verify(mockTypedData, "0x123")
	assert.Error(t, err)
	// ensure the sig is invalid
	assert.False(t, valid)
}
