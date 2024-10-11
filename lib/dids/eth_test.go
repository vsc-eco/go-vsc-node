package dids_test

import (
	"encoding/hex"
	"testing"
	"vsc-node/lib/dids"

	blocks "github.com/ipfs/go-block-format"
	"github.com/ipfs/go-cid"
	"github.com/stretchr/testify/assert"
)

func TestVerifyExternalSigCIDKeyAndBytes(t *testing.T) {
	// external CID, eth addr, and sig provided
	cidStr := "bafyreibgojpwsyovx2idjo5vz75f5gf4uevegoansmx3jx4exy5zggbmre"
	signatureHex := "0x05b806a4c8c091d604049f8333aad796c9b707cffa5c154a2d0f6d310080103b5f2d5816121806bd349eb6b206ac0d1048c787bea8325ccb3b8963ddee66e1541c"
	ethAddress := "0x88EBB64C264AFf10141149F9770F8D644C9D86C5"

	// raw CBOR message bytes
	msg := []byte{
		0xa4, 0x62, 0x74, 0x78, 0xa2, 0x62, 0x6f, 0x70, 0x68, 0x74, 0x72, 0x61,
		0x6e, 0x73, 0x66, 0x65, 0x72, 0x67, 0x70, 0x61, 0x79, 0x6c, 0x6f, 0x61,
		0x64, 0xa4, 0x62, 0x74, 0x6b, 0x64, 0x48, 0x49, 0x56, 0x45, 0x62, 0x74,
		0x6f, 0x78, 0x3b, 0x64, 0x69, 0x64, 0x3a, 0x70, 0x6b, 0x68, 0x3a, 0x65,
		0x69, 0x70, 0x31, 0x35, 0x35, 0x3a, 0x31, 0x3a, 0x30, 0x78, 0x38, 0x38,
		0x45, 0x42, 0x42, 0x36, 0x34, 0x43, 0x32, 0x36, 0x34, 0x41, 0x46, 0x66,
		0x31, 0x30, 0x31, 0x34, 0x31, 0x31, 0x34, 0x39, 0x46, 0x39, 0x37, 0x37,
		0x30, 0x46, 0x38, 0x44, 0x36, 0x34, 0x34, 0x43, 0x39, 0x44, 0x38, 0x36,
		0x43, 0x35, 0x64, 0x66, 0x72, 0x6f, 0x6d, 0x78, 0x3b, 0x64, 0x69, 0x64,
		0x3a, 0x70, 0x6b, 0x68, 0x3a, 0x65, 0x69, 0x70, 0x31, 0x35, 0x35, 0x3a,
		0x31, 0x3a, 0x30, 0x78, 0x38, 0x38, 0x45, 0x42, 0x42, 0x36, 0x34, 0x43,
		0x32, 0x36, 0x34, 0x41, 0x46, 0x66, 0x31, 0x30, 0x31, 0x34, 0x31, 0x31,
		0x34, 0x39, 0x46, 0x39, 0x37, 0x37, 0x30, 0x46, 0x38, 0x44, 0x36, 0x34,
		0x34, 0x43, 0x39, 0x44, 0x38, 0x36, 0x43, 0x35, 0x66, 0x61, 0x6d, 0x6f,
		0x75, 0x6e, 0x74, 0x01, 0x63, 0x5f, 0x5f, 0x74, 0x66, 0x76, 0x73, 0x63,
		0x2d, 0x74, 0x78, 0x63, 0x5f, 0x5f, 0x76, 0x63, 0x30, 0x2e, 0x32, 0x67,
		0x68, 0x65, 0x61, 0x64, 0x65, 0x72, 0x73, 0xa4, 0x64, 0x74, 0x79, 0x70,
		0x65, 0x01, 0x65, 0x6e, 0x6f, 0x6e, 0x63, 0x65, 0x0d, 0x67, 0x69, 0x6e,
		0x74, 0x65, 0x6e, 0x74, 0x73, 0x80, 0x6e, 0x72, 0x65, 0x71, 0x75, 0x69,
		0x72, 0x65, 0x64, 0x5f, 0x61, 0x75, 0x74, 0x68, 0x73, 0x81, 0x78, 0x3b,
		0x64, 0x69, 0x64, 0x3a, 0x70, 0x6b, 0x68, 0x3a, 0x65, 0x69, 0x70, 0x31,
		0x35, 0x35, 0x3a, 0x31, 0x3a, 0x30, 0x78, 0x38, 0x38, 0x45, 0x42, 0x42,
		0x36, 0x34, 0x43, 0x32, 0x36, 0x34, 0x41, 0x46, 0x66, 0x31, 0x30, 0x31,
		0x34, 0x31, 0x31, 0x34, 0x39, 0x46, 0x39, 0x37, 0x37, 0x30, 0x46, 0x38,
		0x44, 0x36, 0x34, 0x34, 0x43, 0x39, 0x44, 0x38, 0x36, 0x43, 0x35,
	}

	// decode the provided signature and CID
	sig, err := hex.DecodeString(signatureHex[2:])
	assert.Nil(t, err)

	cidDecoded, err := cid.Decode(cidStr)
	assert.Nil(t, err)

	// create block with CBOR data and CID
	block, err := blocks.NewBlockWithCid(msg, cidDecoded)
	assert.Nil(t, err)

	// verify block CID matches provided CID
	assert.Equal(t, block.Cid().String(), cidStr)

	// check if generated CID matches expected CID
	generatedCID := cid.NewCidV1(0x71, block.Multihash())
	assert.Equal(t, generatedCID.String(), cidStr)

	// verify the signature using EthDID
	ethDID := dids.NewEthDID(ethAddress)
	sigStr := "0x" + hex.EncodeToString(sig)
	isValid, err := ethDID.Verify(block, sigStr)
	assert.Nil(t, err)
	assert.True(t, isValid)
}

// func TestEthDIDVerify(t *testing.T) {
// 	// data to be signed and verified
// 	data := map[string]any{
// 		// we have 2 fields of data to ensure deterministic encoding and decoding
// 		//
// 		// this is because if our encoding and decoding is not deterministic, the signature
// 		// will differ each time we sign this data because the order will switch since Go maps
// 		// don't guarantee order
// 		"foo": "bar",
// 		"baz": 12345,
// 	}

// 	// encode data into CBOR and create a block
// 	cborData, err := cbor.WrapObject(data, multihash.SHA2_256, -1)
// 	assert.Nil(t, err)

// 	// create a block with the CBOR data
// 	block, err := blocks.NewBlockWithCid(cborData.RawData(), cborData.Cid())
// 	assert.Nil(t, err)

// 	// gen a priv key for signing
// 	privateKey, err := crypto.GenerateKey()
// 	assert.Nil(t, err)

// 	// create new provider capable of signing
// 	provider, err := dids.NewEthProvider(privateKey)
// 	assert.Nil(t, err)

// 	// sign the block
// 	sig, err := provider.Sign(block)
// 	assert.Nil(t, err)

// 	// verify the sig using the EthDID
// 	ethDID := dids.NewEthDID(crypto.PubkeyToAddress(privateKey.PublicKey).Hex())
// 	isValid, err := ethDID.Verify(block, sig)
// 	assert.Nil(t, err)
// 	assert.True(t, isValid)
// }

// func TestNewEthDID(t *testing.T) {
// 	ethAddr := "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC"
// 	did := dids.NewEthDID(ethAddr)

// 	expectedDID := dids.EthDIDPrefix + ethAddr
// 	assert.Equal(t, expectedDID, did.String())
// }

// func TestConvertToEIP712TypedDataInvalidDomain(t *testing.T) {
// 	data := map[string]interface{}{"name": "Alice"}

// 	_, err := dids.ConvertToEIP712TypedData("", data, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})
// 	assert.NotNil(t, err)
// }

// func TestConvertToEIP712TypedDataInvalidPrimaryTypename(t *testing.T) {
// 	data := map[string]interface{}{"name": "Alice"}

// 	_, err := dids.ConvertToEIP712TypedData("vsc.network", data, "", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})
// 	assert.NotNil(t, err)
// }

// func TestEIP712InvalidTypes(t *testing.T) {
// 	data := map[string]interface{}{
// 		"myFunc": func() {},
// 		"myChan": make(chan int),
// 	}

// 	_, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})

// 	// invalid types SHOULD throw errors
// 	assert.NotNil(t, err)
// }

// func TestEIP712ComplexSliceArrayData(t *testing.T) {
// 	// we need to be able to confirm these types in the EIP-712 typed data, since they are difficult edge cases
// 	data := map[string]interface{}{
// 		"names":        []interface{}{"Alice", "Bob"},
// 		"ages":         []interface{}{25, 30},
// 		"someByteData": []byte{0x01, 0x02, 0x03},
// 		"marks":        []interface{}{25.5, 30.5},
// 	}

// 	// convert data into EIP-712 typed data
// 	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})
// 	assert.Nil(t, err)

// 	// marshal the output for manual field assertions
// 	marshalled, err := typedData.MarshalJSON()
// 	assert.Nil(t, err)

// 	// unmarshal the output for easier assertions
// 	var result map[string]interface{}
// 	err = json.Unmarshal(marshalled, &result)
// 	assert.Nil(t, err)

// 	// assert that the types field exists
// 	typesField, ok := result["types"].(map[string]interface{})
// 	assert.True(t, ok)

// 	// ensure tx_container_v0 field exists in types
// 	txContainerField, ok := typesField["tx_container_v0"].([]interface{})
// 	assert.True(t, ok)

// 	// helper func to find a type by name
// 	findFieldsType := func(fields []interface{}, name string) string {
// 		for _, field := range fields {
// 			fieldMap, ok := field.(map[string]interface{})
// 			if ok && fieldMap["name"] == name {
// 				return fieldMap["type"].(string)
// 			}
// 		}
// 		return ""
// 	}

// 	// assert the types of the fields match the expected types
// 	assert.Equal(t, "string[]", findFieldsType(txContainerField, "names"))
// 	assert.Equal(t, "int256[]", findFieldsType(txContainerField, "ages"))
// 	assert.Equal(t, "uint256[]", findFieldsType(txContainerField, "marks"))
// 	assert.Equal(t, "bytes", findFieldsType(txContainerField, "someByteData"))

// 	// assert the primary type matches what we expect
// 	assert.Equal(t, "tx_container_v0", result["primaryType"])

// 	// assert domain matches
// 	domainField, ok := result["domain"].(map[string]interface{})
// 	assert.True(t, ok)
// 	assert.Equal(t, domainField["name"], "vsc.network")
// }

// func TestCreateEthDIDProvider(t *testing.T) {
// 	privKey, err := crypto.GenerateKey()
// 	assert.Nil(t, err)
// 	provider, err := dids.NewEthProvider(privKey)
// 	assert.Nil(t, err)
// 	assert.NotNil(t, provider)
// }

// func TestCreateEthDIDProviderInvalidKey(t *testing.T) {
// 	_, err := dids.NewEthProvider(nil)
// 	assert.NotNil(t, err)
// }

// // exact real data case from the Bitcoin wrapper UI
// func TestEIP712RealDataCase(t *testing.T) {
// 	// structure to match the required JSON schema for a tx on the Bitoin wrapper UI: https://github.com/vsc-eco/Bitcoin-wrap-UI
// 	// with some potentially sensitive data replaced with XXXXXs and YYYYYs
// 	data := map[string]interface{}{
// 		"tx": map[string]interface{}{
// 			"op": "transfer",
// 			"payload": map[string]interface{}{
// 				"tk":     "HIVE",
// 				"to":     "hive:XXXXX",
// 				"from":   "did:pkh:eip155:1:YYYYY",
// 				"amount": uint64(1),
// 			},
// 		},
// 		"__t": "vsc-tx",
// 		"__v": "0.2",
// 		"headers": map[string]interface{}{
// 			"type":    uint64(1),
// 			"nonce":   uint64(1),
// 			"intents": []interface{}{},
// 			"required_auths": []string{
// 				"did:pkh:eip155:1:YYYYY",
// 			},
// 		},
// 	}

// 	// convert data into EIP-712 typed data
// 	typedDataFromOurConversion, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})
// 	assert.Nil(t, err)

// 	// a real inspected transaction from the Bitcoin wrapper UI (https://github.com/vsc-eco/Bitcoin-wrap-UI)
// 	// that we try to "match" exactly via the EIP-712 typed data input that we create above (data var)
// 	//
// 	// except the username of the tx and the DIDs are different for privacy reasons (XXXXXs and YYYYYs)
// 	realSystemTypedData := `
// {
//     "EIP712Domain": [
//         {
//             "name": "name",
//             "type": "string"
//         }
//     ],
//     "types": {
//         "tx_container_v0.tx.payload": [
//             {
//                 "name": "tk",
//                 "type": "string"
//             },
//             {
//                 "name": "to",
//                 "type": "string"
//             },
//             {
//                 "name": "from",
//                 "type": "string"
//             },
//             {
//                 "name": "amount",
//                 "type": "uint256"
//             }
//         ],
//         "tx_container_v0.tx": [
//             {
//                 "name": "op",
//                 "type": "string"
//             },
//             {
//                 "name": "payload",
//                 "type": "tx_container_v0.tx.payload"
//             }
//         ],
//         "tx_container_v0.headers": [
//             {
//                 "name": "type",
//                 "type": "uint256"
//             },
//             {
//                 "name": "nonce",
//                 "type": "uint256"
//             },
//             {
//                 "name": "intents",
//                 "type": "undefined[]"
//             },
//             {
//                 "name": "required_auths",
//                 "type": "string[]"
//             }
//         ],
//         "tx_container_v0": [
//             {
//                 "name": "tx",
//                 "type": "tx_container_v0.tx"
//             },
//             {
//                 "name": "__t",
//                 "type": "string"
//             },
//             {
//                 "name": "__v",
//                 "type": "string"
//             },
//             {
//                 "name": "headers",
//                 "type": "tx_container_v0.headers"
//             }
//         ]
//     },
//     "primaryType": "tx_container_v0",
//     "message": {
//         "tx": {
//             "op": "transfer",
//             "payload": {
//                 "tk": "HIVE",
//                 "to": "hive:XXXXX",
//                 "from": "did:pkh:eip155:1:YYYYY",
//                 "amount": 1
//             }
//         },
//         "__t": "vsc-tx",
//         "__v": "0.2",
//         "headers": {
//             "type": 1,
//             "nonce": 1,
//             "intents": [],
//             "required_auths": [
//                 "did:pkh:eip155:1:YYYYY"
//             ]
//         }
//     },
//     "domain": {
//         "name": "vsc.network"
//     }
// }
// 	`

// 	var realSystemTypedDataMap map[string]interface{}
// 	err = json.Unmarshal([]byte(realSystemTypedData), &realSystemTypedDataMap)
// 	assert.Nil(t, err)

// 	// marshal and unmarshal our typed data conversion for sake of comparision
// 	marshalled, err := typedDataFromOurConversion.MarshalJSON()
// 	assert.Nil(t, err)

// 	var typedDataFromOurConversionMap map[string]interface{}
// 	err = json.Unmarshal(marshalled, &typedDataFromOurConversionMap)
// 	assert.Nil(t, err)

// 	// custom comparison for map to define equality without caring about order
// 	opts := cmp.Options{
// 		cmpopts.SortSlices(func(x, y interface{}) bool {
// 			xMap, xOk := x.(map[string]interface{})
// 			yMap, yOk := y.(map[string]interface{})
// 			if xOk && yOk {
// 				if xVal, xExists := xMap["name"]; xExists {
// 					if yVal, yExists := yMap["name"]; yExists {
// 						return fmt.Sprintf("%v", xVal) < fmt.Sprintf("%v", yVal)
// 					}
// 				}
// 			}
// 			// if not not a map, fallback to basic comparison of strings
// 			return fmt.Sprintf("%v", x) < fmt.Sprintf("%v", y)
// 		}),
// 		cmpopts.EquateEmpty(), // we consider empty slices equal to nil slices for this
// 	}

// 	// if there's no diff, the test passes
// 	assert.Equal(t, "", cmp.Diff(realSystemTypedDataMap, typedDataFromOurConversionMap, opts...))
// }

// func TestEIP712FloatHandlerError(t *testing.T) {
// 	// random valid data
// 	data := map[string]interface{}{
// 		"age": 1.5, // float data which will cause the handler error
// 	}

// 	// we expect an error because we have a float in our data and we specify in our
// 	// handler that we don't want floats
// 	_, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return nil, fmt.Errorf("we decide to throw this error if we accidently put a float in our data")
// 	})
// 	assert.NotNil(t, err)
// }

// func TestEIP712EmptyData(t *testing.T) {
// 	data := map[string]interface{}{}

// 	// convert data into EIP-712 typed data
// 	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})
// 	assert.Nil(t, err)

// 	// marshal the output for manual field assertions
// 	marshalled, err := typedData.MarshalJSON()
// 	assert.Nil(t, err)

// 	// unmarshal the JSON into a map for individual field checks
// 	var result map[string]interface{}
// 	err = json.Unmarshal(marshalled, &result)
// 	assert.Nil(t, err)

// 	// check types
// 	typesField, ok := result["types"].(map[string]interface{})
// 	assert.True(t, ok)
// 	assert.Contains(t, typesField, "tx_container_v0")
// 	assert.Empty(t, typesField["tx_container_v0"])

// 	// check primaryType
// 	assert.Equal(t, result["primaryType"], "tx_container_v0")

// 	// check domain
// 	domainField, ok := result["domain"].(map[string]interface{})
// 	assert.True(t, ok)
// 	assert.Equal(t, domainField["name"], "vsc.network")

// 	// check message
// 	messageField, ok := result["message"].(map[string]interface{})
// 	assert.True(t, ok)
// 	assert.Empty(t, messageField)

// 	// check EIP712Domain
// 	eip712Domain, ok := result["EIP712Domain"].([]interface{})
// 	assert.True(t, ok)
// 	assert.Len(t, eip712Domain, 1)

// 	domainFieldEntry, ok := eip712Domain[0].(map[string]interface{})
// 	assert.True(t, ok)
// 	assert.Equal(t, domainFieldEntry["name"], "name")
// 	assert.Equal(t, domainFieldEntry["type"], "string")
// }

// func TestEIP712StructData(t *testing.T) {
// 	dummyStruct := struct {
// 		Name string
// 	}{
// 		Name: "alice",
// 	}

// 	// convert data into EIP-712 typed data
// 	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", dummyStruct, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})
// 	assert.Nil(t, err)

// 	// assert that alice exists in the msg field
// 	marshalled, err := typedData.MarshalJSON()
// 	assert.Nil(t, err)

// 	var result map[string]interface{}
// 	err = json.Unmarshal(marshalled, &result)
// 	assert.Nil(t, err)

// 	// confirm message field has the correct name and value
// 	messageField, ok := result["message"].(map[string]interface{})
// 	assert.True(t, ok)
// 	assert.Contains(t, messageField, "Name")
// 	assert.Equal(t, messageField["Name"], "alice")
// }

// func TestEIP712ConvertStringToAddr(t *testing.T) {
// 	// data that is of type string, that should auto-coerce to an address to match EIP-712 spec
// 	data := map[string]interface{}{
// 		"wallet": "0xCcCCccccCCCCcCCCCCCcCcCccCcCCCcCcccccccC",
// 	}

// 	typedData, err := dids.ConvertToEIP712TypedData("vsc.network", data, "tx_container_v0", func(f float64) (*big.Int, error) {
// 		return big.NewInt(int64(f)), nil
// 	})
// 	assert.Nil(t, err)

// 	// marshal the output for manual field assertions
// 	marshalled, err := typedData.MarshalJSON()
// 	assert.Nil(t, err)

// 	// unmarshal the JSON into a map for individual field checks
// 	var result map[string]interface{}
// 	err = json.Unmarshal(marshalled, &result)
// 	assert.Nil(t, err)

// 	// ensure types exists and is in the correct form
// 	typesField, ok := result["types"].(map[string]interface{})
// 	assert.True(t, ok)

// 	// ensure the tx_container_v0 field is present and is a slice
// 	txContainerField, ok := typesField["tx_container_v0"].([]interface{})
// 	assert.True(t, ok)

// 	// loop through the fields in the tx_container_v0 to find wallet and check its type
// 	var walletField map[string]interface{}
// 	for _, field := range txContainerField {
// 		fieldMap, ok := field.(map[string]interface{})
// 		if ok && fieldMap["name"] == "wallet" {
// 			walletField = fieldMap
// 			break
// 		}
// 	}

// 	// ensure wallet is found and is of type address instead of the initial string
// 	assert.NotNil(t, walletField)
// 	assert.Contains(t, walletField, "type")
// 	assert.Equal(t, walletField["type"], "address")
// }
