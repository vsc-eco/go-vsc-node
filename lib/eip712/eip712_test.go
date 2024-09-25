package eip712_test

import (
	"testing"
	"vsc-node/lib/eip712"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"github.com/stretchr/testify/assert"
)

var (
	// using some dummy values found: https://gist.github.com/APTy/f2a6864a97889793c587635b562c7d72 (MIT license)
	mockData = eip712.TypedData{
		Domain: eip712.EIP712Domain{
			Name:              "vsc dapp",
			Version:           "v1.0.0",
			ChainId:           uint256.Int{1},
			VerifyingContract: "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
		},
		PrimaryType: "AuthRequest",
		Message: map[string][]eip712.FieldWithType{
			"prompt":    {{Name: "prompt", Type: "string"}},
			"createdAt": {{Name: "createdAt", Type: "uint256"}},
		},
		Types: map[string][]eip712.FieldWithType{
			"AuthRequest": {
				{Name: "prompt", Type: "string"},
				{Name: "createdAt", Type: "uint256"},
			},
		},
	}
)

func TestSigningData(t *testing.T) {
	// mock data
	typedData := mockData

	// some priv key
	privKey, err := crypto.GenerateKey()
	assert.Nil(t, err)

	// sign with this key
	sig, err := eip712.SignTypedData(typedData, privKey)
	assert.Nil(t, err)

	// ensure sig isn't empty
	assert.NotEmpty(t, sig)
}

func TestRecoveringSigner(t *testing.T) {
	// mock data
	typedData := mockData

	// some priv key
	privKey, err := crypto.GenerateKey()
	assert.Nil(t, err)

	// sign with the key
	sig, err := eip712.SignTypedData(typedData, privKey)
	assert.Nil(t, err)

	// recover ethereum addr from the sig
	recoveredAddress, err := eip712.RecoverAddrOfSigner(typedData, sig)
	assert.Nil(t, err)

	// using the crypto pkg to convert the pub key to a hex address, we need
	// to ensure our recovered address from the signature of signing typed data
	// matches this one exactly
	assert.Equal(t, crypto.PubkeyToAddress(privKey.PublicKey).Hex(), recoveredAddress)
}

func TestHashing(t *testing.T) {
	// mock data
	typedData := mockData

	// simply hash the typed data alongside the domain separator
	hash, err := eip712.HashTypedData(typedData)
	// no errors and non-empty hash
	assert.Nil(t, err)
	assert.NotEmpty(t, hash)
}

func TestTypeValidation(t *testing.T) {
	// mock data
	typedData := mockData

	// no errors
	assert.Nil(t, typedData.ValidateTypes())

	// some valid types

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "someRandomNonExistentTypeThatIsInvalid"}}
	assert.NotNil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "uint256"}}
	assert.Nil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "uint25632"}}
	assert.NotNil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "string"}}
	assert.Nil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "dog"}}
	assert.NotNil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "address"}}
	assert.Nil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "bool"}}
	assert.Nil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "bytes"}}
	assert.Nil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "bytes32"}}
	assert.Nil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "uint256[]"}}
	assert.Nil(t, typedData.ValidateTypes())

	typedData.Message["foo"] = []eip712.FieldWithType{{Name: "bar", Type: "uint256[2]"}}
	assert.Nil(t, typedData.ValidateTypes())
}

func TestDomainSeparatorUniqueness(t *testing.T) {
	// mock data
	typedData := mockData

	// ensure domain separator is generated correctly
	domainSeparator := typedData.Domain.DomainSeparator()
	assert.NotEmpty(t, domainSeparator)

	// ensure with the same data but different chain id, the domain separator is different as a sanity check
	typedData.Domain.ChainId = uint256.Int{2}
	domainSeparator2 := typedData.Domain.DomainSeparator()
	assert.NotEmpty(t, domainSeparator2)
	assert.NotEqual(t, domainSeparator, domainSeparator2)
}

func TestDomainSeparatorDeterminism(t *testing.T) {
	typedData1 := mockData
	typedData2 := mockData

	// ensure domain separator is generated correctly
	domainSeparator1 := typedData1.Domain.DomainSeparator()
	domainSeparator2 := typedData2.Domain.DomainSeparator()
	assert.Equal(t, domainSeparator1, domainSeparator2)
	assert.NotEmpty(t, domainSeparator1) // implicitly checks both since at this point they have to be the same anyway
}

func TestTamperingWithData(t *testing.T) {
	// mock data
	typedData := mockData

	// gen a priv key for signing
	privKey, err := crypto.GenerateKey()
	assert.Nil(t, err)

	// sign the original typed data
	sig, err := eip712.SignTypedData(typedData, privKey)
	assert.Nil(t, err)

	// ensure the original signature validates correctly
	recoveredAddr1, err := eip712.RecoverAddrOfSigner(typedData, sig)
	assert.Nil(t, err)
	ok, err := eip712.IsValidSignature(typedData, sig, recoveredAddr1)
	assert.Nil(t, err)
	assert.True(t, ok)

	// tampering with the data!
	typedData.Message["prompt"] = []eip712.FieldWithType{{Name: "prompt", Type: "uint256"}}

	ok, err = eip712.IsValidSignature(typedData, sig, recoveredAddr1)
	assert.Nil(t, err)
	assert.False(t, ok)
}
