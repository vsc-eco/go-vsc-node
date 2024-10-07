package dids_test

import (
	"math/big"
	"testing"
	"vsc-node/lib/dids"

	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	"github.com/stretchr/testify/assert"
)

func TestNewEthDID(t *testing.T) {
	ethAddr := "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"
	did := dids.NewEthDID(ethAddr)

	expectedDID := dids.EthDIDPrefix + ethAddr
	assert.Equal(t, expectedDID, did.String())
}

func TestEIP712Data(t *testing.T) {
	data := map[string]interface{}{
		"Name":       "alice",
		"Tags":       []string{"tag1", "tag2"},
		"Age":        []int{30, 40},
		"Identifier": [32]byte{0xaa, 0xbb, 0xcc},
		"Extra": map[string]interface{}{
			"Hobbies": []string{"coding", "reading"},
			"Height":  170,
			"Data":    []byte{0x01, 0x02, 0x03},
		},
	}

	// convert data into EIP-712 typed data
	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	assert.Nil(t, err)

	// expected  message map to compare against
	//
	// expecting some vals to be strings due to the serialization
	expectedMessage := map[string]interface{}{
		"Name":       "alice",
		"Tags":       []string{"tag1", "tag2"},
		"Age":        []string{"30", "40"},
		"Identifier": [32]byte{0xaa, 0xbb, 0xcc},
		"Extra": map[string]interface{}{
			"Hobbies": []string{"coding", "reading"},
			"Height":  "170",
			"Data":    []byte{0x01, 0x02, 0x03},
		},
	}

	// check the message contents
	assert.Equal(t, expectedMessage, typedData.Data.Message)

	// expected types for comparison
	expectedTypes := apitypes.Types{
		"EIP712Domain": []apitypes.Type{
			{Name: "name", Type: "string"},
		},
		"ExtraMapInterface": []apitypes.Type{
			{Name: "Hobbies", Type: "string[]"},
			{Name: "Height", Type: "int256"},
			{Name: "Data", Type: "bytes"},
		},
		"tx_container_v0": []apitypes.Type{
			{Name: "Name", Type: "string"},
			{Name: "Tags", Type: "string[]"},
			{Name: "Age", Type: "int256[]"},
			{Name: "Identifier", Type: "bytes"},
			{Name: "Extra", Type: "ExtraMapInterface"},
		},
	}

	// confirm types without checking order!
	assert.ElementsMatch(t, expectedTypes["EIP712Domain"], typedData.Data.Types["EIP712Domain"])
	assert.ElementsMatch(t, expectedTypes["ExtraMapInterface"], typedData.Data.Types["ExtraMapInterface"])
	assert.ElementsMatch(t, expectedTypes["tx_container_v0"], typedData.Data.Types["tx_container_v0"])

}
