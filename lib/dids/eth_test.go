package dids_test

import (
	"encoding/json"
	"fmt"
	"math/big"
	"testing"
	"vsc-node/lib/dids"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"github.com/stretchr/testify/assert"
)

func TestNewEthDID(t *testing.T) {
	ethAddr := "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"
	did := dids.NewEthDID(ethAddr)

	expectedDID := dids.EthDIDPrefix + ethAddr
	assert.Equal(t, expectedDID, did.String())
}

func TestEIP712RealDataCase(t *testing.T) {
	// structure to match the required JSON schema for a tx on the Bitoin wrapper UI: https://github.com/vsc-eco/Bitcoin-wrap-UI
	// with some potentially sensitive data replaced with XXXXXs and YYYYYs
	data := map[string]interface{}{
		"tx": map[string]interface{}{
			"op": "transfer",
			"payload": map[string]interface{}{
				"tk":     "HIVE",
				"to":     "hive:XXXXX",
				"from":   "did:pkh:eip155:1:YYYYY",
				"amount": 1,
			},
		},
		"__t": "vsc-tx",
		"__v": "0.2",
		"headers": map[string]interface{}{
			"type":    1,
			"nonce":   1,
			"intents": []interface{}{},
			"required_auths": []string{
				"did:pkh:eip155:1:YYYYY",
			},
		},
	}

	// convert data into EIP-712 typed data
	typedDataFromOurConversion, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	assert.Nil(t, err)

	// a real inspected transaction from the Bitcoin wrapper UI (https://github.com/vsc-eco/Bitcoin-wrap-UI)
	// that we try to "match" exactly via the EIP-712 typed data input that we create above (data var)
	//
	// except the username of the tx and the DIDs are different for privacy reasons (XXXXXs and YYYYYs)
	realSystemTypedData := `
{
    "EIP712Domain": [
        {
            "name": "name",
            "type": "string"
        }
    ],
    "types": {
        "tx_container_v0.tx.payload": [
            {
                "name": "tk",
                "type": "string"
            },
            {
                "name": "to",
                "type": "string"
            },
            {
                "name": "from",
                "type": "string"
            },
            {
                "name": "amount",
                "type": "uint256"
            }
        ],
        "tx_container_v0.tx": [
            {
                "name": "op",
                "type": "string"
            },
            {
                "name": "payload",
                "type": "tx_container_v0.tx.payload"
            }
        ],
        "tx_container_v0.headers": [
            {
                "name": "type",
                "type": "uint256"
            },
            {
                "name": "nonce",
                "type": "uint256"
            },
            {
                "name": "intents",
                "type": "undefined[]"
            },
            {
                "name": "required_auths",
                "type": "string[]"
            }
        ],
        "tx_container_v0": [
            {
                "name": "tx",
                "type": "tx_container_v0.tx"
            },
            {
                "name": "__t",
                "type": "string"
            },
            {
                "name": "__v",
                "type": "string"
            },
            {
                "name": "headers",
                "type": "tx_container_v0.headers"
            }
        ]
    },
    "primaryType": "tx_container_v0",
    "message": {
        "tx": {
            "op": "transfer",
            "payload": {
                "tk": "HIVE",
                "to": "hive:XXXXX",
                "from": "did:pkh:eip155:1:YYYYY",
                "amount": 1
            }
        },
        "__t": "vsc-tx",
        "__v": "0.2",
        "headers": {
            "type": 1,
            "nonce": 1,
            "intents": [],
            "required_auths": [
                "did:pkh:eip155:1:YYYYY"
            ]
        }
    },
    "domain": {
        "name": "vsc.network"
    }
}
	`

	var realSystemTypedDataMap map[string]interface{}
	err = json.Unmarshal([]byte(realSystemTypedData), &realSystemTypedDataMap)
	assert.Nil(t, err)

	// marshal and unmarshal our typed data conversion for sake of comparision
	marshalled, err := typedDataFromOurConversion.MarshalJSON()
	assert.Nil(t, err)

	var typedDataFromOurConversionMap map[string]interface{}
	err = json.Unmarshal(marshalled, &typedDataFromOurConversionMap)
	assert.Nil(t, err)

	// custom comparison for map to define equality without caring about order
	opts := cmp.Options{
		cmpopts.SortSlices(func(x, y interface{}) bool {
			xMap, xOk := x.(map[string]interface{})
			yMap, yOk := y.(map[string]interface{})
			if xOk && yOk {
				if xVal, xExists := xMap["name"]; xExists {
					if yVal, yExists := yMap["name"]; yExists {
						return fmt.Sprintf("%v", xVal) < fmt.Sprintf("%v", yVal)
					}
				}
			}
			// if not not a map, fallback to basic comparison of strings
			return fmt.Sprintf("%v", x) < fmt.Sprintf("%v", y)
		}),
		cmpopts.EquateEmpty(), // we consider empty slices equal to nil slices for this
	}

	// if there's no diff, the test passes
	assert.Equal(t, "", cmp.Diff(realSystemTypedDataMap, typedDataFromOurConversionMap, opts...))
}

func TestEIP712FloatHandlerError(t *testing.T) {
	// random valid data
	data := map[string]interface{}{
		"age": 1.5, // float data which will cause the handler error
	}

	// we expect an error because we have a float in our data and we specify in our
	// handler that we don't want floats
	_, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
		return nil, fmt.Errorf("we decide to throw this error if we accidently put a float in our data")
	})
	assert.NotNil(t, err)
}

func TestEIP712EmptyData(t *testing.T) {
	data := map[string]interface{}{}

	// convert data into EIP-712 typed data
	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	assert.Nil(t, err)

	// marshal the output for manual field assertions
	marshalled, err := typedData.MarshalJSON()
	assert.Nil(t, err)

	// unmarshal the JSON into a map for individual field checks
	var result map[string]interface{}
	err = json.Unmarshal(marshalled, &result)
	assert.Nil(t, err)

	// check types
	typesField, ok := result["types"].(map[string]interface{})
	assert.True(t, ok)
	assert.Contains(t, typesField, "tx_container_v0")
	assert.Empty(t, typesField["tx_container_v0"])

	// check primaryType
	assert.Equal(t, result["primaryType"], "tx_container_v0")

	// check domain
	domainField, ok := result["domain"].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, domainField["name"], "vsc.network")

	// check message
	messageField, ok := result["message"].(map[string]interface{})
	assert.True(t, ok)
	assert.Empty(t, messageField)

	// check EIP712Domain
	eip712Domain, ok := result["EIP712Domain"].([]interface{})
	assert.True(t, ok)
	assert.Len(t, eip712Domain, 1)

	domainFieldEntry, ok := eip712Domain[0].(map[string]interface{})
	assert.True(t, ok)
	assert.Equal(t, domainFieldEntry["name"], "name")
	assert.Equal(t, domainFieldEntry["type"], "string")
}

func TestEIP712StructData(t *testing.T) {
	dummyStruct := struct {
		Name string
	}{
		Name: "alice",
	}

	// convert data into EIP-712 typed data
	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", dummyStruct, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	assert.Nil(t, err)

	// assert that alice exists in the msg field
	marshalled, err := typedData.MarshalJSON()
	assert.Nil(t, err)

	var result map[string]interface{}
	err = json.Unmarshal(marshalled, &result)
	assert.Nil(t, err)

	// confirm message field has the correct name and value
	messageField, ok := result["message"].(map[string]interface{})
	assert.True(t, ok)
	assert.Contains(t, messageField, "Name")
	assert.Equal(t, messageField["Name"], "alice")
}

func TestEIP712ConvertStringToAddr(t *testing.T) {
	// data that is of type string, that should auto-coerce to an address to match EIP-712 spec
	data := map[string]interface{}{
		"wallet": "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
	}

	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	assert.Nil(t, err)

	// marshal the output for manual field assertions
	marshalled, err := typedData.MarshalJSON()
	assert.Nil(t, err)

	// unmarshal the JSON into a map for individual field checks
	var result map[string]interface{}
	err = json.Unmarshal(marshalled, &result)
	assert.Nil(t, err)

	// ensure types exists and is in the correct form
	typesField, ok := result["types"].(map[string]interface{})
	assert.True(t, ok)

	// ensure the tx_container_v0 field is present and is a slice
	txContainerField, ok := typesField["tx_container_v0"].([]interface{})
	assert.True(t, ok)

	// loop through the fields in the tx_container_v0 to find wallet and check its type
	var walletField map[string]interface{}
	for _, field := range txContainerField {
		fieldMap, ok := field.(map[string]interface{})
		if ok && fieldMap["name"] == "wallet" {
			walletField = fieldMap
			break
		}
	}

	// ensure wallet is found and is of type address instead of the initial string
	assert.NotNil(t, walletField)
	assert.Contains(t, walletField, "type")
	assert.Equal(t, walletField["type"], "address")
}
