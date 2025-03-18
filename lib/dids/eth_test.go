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
	cidStr := "bafyreiglzf54t4pfhn6du6ez77oi7dseba52yii7hbq77tlpasvjkzfde4"
	signatureHex := "0x1e5c6be56c5a2915958f3b663f0831331111e2b1e0ac3e9aec2f7b497229d0f810014789193da635dc35be1ae6dbd5be5250c0f020b67f9fa2e7abe387f7d88e1c"
	ethAddress := "0x88EBB64C264AFf10141149F9770F8D644C9D86C5"

	// raw CBOR message bytes
	msg := []byte{
		164, 98, 116, 120, 162, 98, 111, 112, 104, 119, 105, 116,
		104, 100, 114, 97, 119, 103, 112, 97, 121, 108, 111, 97,
		100, 164, 98, 116, 107, 100, 72, 73, 86, 69, 98, 116,
		111, 109, 104, 105, 118, 101, 58, 103, 101, 111, 53, 50,
		114, 101, 121, 100, 102, 114, 111, 109, 120, 59, 100, 105,
		100, 58, 112, 107, 104, 58, 101, 105, 112, 49, 53, 53,
		58, 49, 58, 48, 120, 56, 56, 69, 66, 66, 54, 52,
		67, 50, 54, 52, 65, 70, 102, 49, 48, 49, 52, 49,
		49, 52, 57, 70, 57, 55, 55, 48, 70, 56, 68, 54,
		52, 52, 67, 57, 68, 56, 54, 67, 53, 102, 97, 109,
		111, 117, 110, 116, 1, 99, 95, 95, 116, 102, 118, 115,
		99, 45, 116, 120, 99, 95, 95, 118, 99, 48, 46, 50,
		103, 104, 101, 97, 100, 101, 114, 115, 164, 100, 116, 121,
		112, 101, 1, 101, 110, 111, 110, 99, 101, 16, 103, 105,
		110, 116, 101, 110, 116, 115, 128, 110, 114, 101, 113, 117,
		105, 114, 101, 100, 95, 97, 117, 116, 104, 115, 129, 120,
		59, 100, 105, 100, 58, 112, 107, 104, 58, 101, 105, 112,
		49, 53, 53, 58, 49, 58, 48, 120, 56, 56, 69, 66,
		66, 54, 52, 67, 50, 54, 52, 65, 70, 102, 49, 48,
		49, 52, 49, 49, 52, 57, 70, 57, 55, 55, 48, 70,
		56, 68, 54, 52, 52, 67, 57, 68, 56, 54, 67, 53,
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
	/*
		js encoded: 0x90f55a770a52909974ce8a033f5e060c6046a1a628234c2e61a8a1d11ff80614596fc75134cb31a04558fc9869ed7ac74f196d396cff907de6ac32216e1d9925fee0390b0d348b125dafff71891fb7a9a70403dbce52e514b5de3888344007cfc1907d585d0b0e66920f6383717e2e9e7c44e42ba86ef49b0e19983ffd702288cc8feccad8246ba9bdd9c600859bb5e43eadfa5224228b775bb152892c4a9386
		go encoded: 0xfec2fa31c80bd2fe1d325dd7ec8de38a40077462095bda76e2030161cf061fb36dc4917ef98e6d005e1d117df7c7b2ad1d9b0bafe8d9e88de8dd477091c25d69fee0390b0d348b125dafff71891fb7a9a70403dbce52e514b5de3888344007cfc1907d585d0b0e66920f6383717e2e9e7c44e42ba86ef49b0e19983ffd702288cd0b2cabc3a4062923b484bac2fdbc5d41a10361ae647122482157869c1dccea

		js type: tx_container_v0(tx_container_v0.tx tx,string __t,string __v,tx_container_v0.headers headers)
		go type: tx_container_v0(tx_container_v0.tx tx,string __t,string __v,tx_container_v0.headers headers)tx_container_v0.headers(uint256 type,uint256 nonce,string[] required_auths)tx_container_v0.tx(string op,tx_container_v0.tx.payload payload)tx_container_v0.tx.payload(string tk,string to,string from,uint256 amount)

		js type 2: tx_container_v0(tx_container_v0_tx tx,string __t,string __v,tx_container_v0_headers headers)tx_container_v0_headers(uint256 type,uint256 nonce,string[] required_auths)tx_container_v0_tx(string op,tx_container_v0_tx_payload payload)tx_container_v0_tx_payload(string tk,string to,string from,uint256 amount)
		go type 2: tx_container_v0(tx_container_v0_tx tx,string __t,string __v,tx_container_v0_headers headers)tx_container_v0_headers(uint256 type,uint256 nonce,string[] required_auths)tx_container_v0_tx(string op,tx_container_v0_tx_payload payload)tx_container_v0_tx_payload(string tk,string to,string from,uint256 amount)

		js encoded 2: 0x8864599cf4366c3385cb45e1243c2cf532675c39037804321ad28a79056e1d75 c76e4779c43a6f4a8969edc6e1f75a8bf27488f2855ea0d941407351f26e4a48 fee0390b0d348b125dafff71891fb7a9a70403dbce52e514b5de3888344007cfc1907d585d0b0e66920f6383717e2e9e7c44e42ba86ef49b0e19983ffd702288a42993297a96fe37c3af6a2b1a1cfe40afb58a7dea98c53d4b55253045cabbc3
		go encoded 2: 0x8864599cf4366c3385cb45e1243c2cf532675c39037804321ad28a79056e1d75 db3d12d880908f53ef36ee1ef35d07abfd40ffdb05ea940facbfa87169c373d6 fee0390b0d348b125dafff71891fb7a9a70403dbce52e514b5de3888344007cfc1907d585d0b0e66920f6383717e2e9e7c44e42ba86ef49b0e19983ffd702288a42993297a96fe37c3af6a2b1a1cfe40afb58a7dea98c53d4b55253045cabbc3
		js encoded 3: 0x8864599cf4366c3385cb45e1243c2cf532675c39037804321ad28a79056e1d75db3d12d880908f53ef36ee1ef35d07abfd40ffdb05ea940facbfa87169c373d6fee0390b0d348b125dafff71891fb7a9a70403dbce52e514b5de3888344007cfc1907d585d0b0e66920f6383717e2e9e7c44e42ba86ef49b0e19983ffd702288a42993297a96fe37c3af6a2b1a1cfe40afb58a7dea98c53d4b55253045cabbc3
							[
						    "0x1901",
							0xb364cbb4ec1c3d3d438ef95f01322f22b04280d481abaa8cd6c7b5c7108f1a7e
						    "0xb364cbb4ec1c3d3d438ef95f01322f22b04280d481abaa8cd6c7b5c7108f1a7e",
						    "0x52c738ef2dbc2a12feaa2b2ff7dde580484533090e826ea4e42c42cded4f5dee"
						]

						{
				    "parts": [
				        "0x1901",
				        "0xb364cbb4ec1c3d3d438ef95f01322f22b04280d481abaa8cd6c7b5c7108f1a7e",
				        "0x52c738ef2dbc2a12feaa2b2ff7dde580484533090e826ea4e42c42cded4f5dee"
				    ],
				    "message": {
				        "tx": {
				            "op": "withdraw",
				            "payload": {
				                "tk": "HIVE",
				                "to": "hive:geo52rey",
				                "from": "did:pkh:eip155:1:0x88EBB64C264AFf10141149F9770F8D644C9D86C5",
				                "amount": 1
				            }
				        },
				        "__t": "vsc-tx",
				        "__v": "0.2",
				        "headers": {
				            "type": 1,
				            "nonce": 16,
				            "intents": [],
				            "required_auths": [
				                "did:pkh:eip155:1:0x88EBB64C264AFf10141149F9770F8D644C9D86C5"
				            ]
				        }
				    },
				    "primaryType": "tx_container_v0",
				    "types": {
				        "EIP712Domain": [
				            {
				                "name": "name",
				                "type": "string"
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
				                "name": "required_auths",
				                "type": "string[]"
				            }
				        ]
				    }
				}

	*/
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
