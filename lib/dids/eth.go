package dids

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"
	"vsc-node/lib/cbor"

	"crypto/ecdsa"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"

	"github.com/go-viper/mapstructure/v2"
	blocks "github.com/ipfs/go-block-format"

	"github.com/vsc-eco/go-ethereum/signer/core/apitypes"

	"github.com/ugorji/go/codec"
)

// ===== constants =====

// matches what is from vsc's system:
// - https://github.com/vsc-eco/Bitcoin-wrap-UI/blob/365d24bc592003be9600f8a0c886e4e6f9bbb1c1/src/hooks/auth/wagmi-web3modal/index.ts#L10
// - https://github.com/w3c-ccg/did-pkh/blob/main/did-pkh-method-draft.md
const EthDIDPrefix = "did:pkh:eip155:1:"

const debug = false

func runDebug(run func() error) error {
	if debug {
		return run()
	}
	return nil
}

// ===== interface assertions =====

// ethr addr | payload type
var _ DID = EthDID("")

var _ Provider[blocks.Block] = &EthProvider{}

// ===== EthDID =====

type EthDID string

func ParseEthDID(did string) (EthDID, error) {
	addr, hasPrefix := strings.CutPrefix(did, EthDIDPrefix)
	if !hasPrefix {
		return "", fmt.Errorf("does not have eth prefix")
	}

	if !common.IsHexAddress(addr) {
		return "", fmt.Errorf("can not represent valid eth address")
	}

	return EthDID(addr), nil
}

func NewEthDID(ethAddr string) EthDID {
	return EthDID(EthDIDPrefix + ethAddr)
}

// ===== implementing the DID interface =====

func (d EthDID) String() string {
	return string(d)
}

func (d EthDID) Identifier() string {
	// returns the ethr address part, like
	// 0x123...
	//
	// remove "did:ethr:" prefix
	return string(d)[len(EthDIDPrefix):]
}

func (d EthDID) Verify(block blocks.Block, sig string) (bool, error) {
	err := runDebug(func() error {
		// decode the block using CBOR into a generic type of map[string]interface
		var decodedData map[string]interface{}
		if err := decodeFromCBOR(block.RawData(), &decodedData); err != nil {
			return fmt.Errorf("failed to decode CBOR data: %v", err)
		}

		var decodedData2 []interface{}
		if err := decodeFromCBOR(block.RawData(), &decodedData2); err != nil {
			return fmt.Errorf("failed to decode CBOR data: %v", err)
		}

		fmt.Println(decodedData2)
		return nil
	})
	if err != nil {
		return false, err
	}

	payload, err := ConvertCBORToEIP712TypedData("vsc.network", block.RawData(), "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	// convert the sorted decoded data into EIP-712 typed data
	// payload, err := ConvertToEIP712TypedData("vsc.network", decodedData, "tx_container_v0", func(f float64) (*big.Int, error) {
	// 	return big.NewInt(int64(f)), nil
	// })

	typeJson, _ := json.Marshal(payload)
	fmt.Println("ConvertCBORToEIP712TypedData payload", payload, string(typeJson))
	if err != nil {
		return false, fmt.Errorf("failed to convert block to EIP-712 typed data: %v", err)
	}

	// compute the EIP-712 hash
	dataHash, err := computeEIP712Hash(payload.Data)

	fmt.Println("DATA HASH: ", hex.EncodeToString(dataHash))
	if err != nil {
		return false, fmt.Errorf("failed to compute EIP-712 hash: %v", err)
	}

	// decode the sig from the hex
	sigBytes, err := hexutil.Decode(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %v", err)
	}

	if sigBytes[64] != 0 && sigBytes[64] != 1 {
		sigBytes[64] -= 27
	}

	// recover the pub key from the signature and data hash
	sigPubkey, err := ethCrypto.Ecrecover(dataHash, sigBytes)
	if err != nil {
		return false, fmt.Errorf("failed to recover public key from signature: %v", err)
	}

	pubKey, err := ethCrypto.UnmarshalPubkey(sigPubkey)
	if err != nil {
		return false, fmt.Errorf("%v", err)
	}

	// extract the recovered addr
	recoveredAddress := ethCrypto.PubkeyToAddress(*pubKey).Hex()

	// get the expected addr from the DID
	expectedAddress := d.Identifier()

	err = runDebug(func() error {
		fmt.Println("RECOVERED: ", recoveredAddress)
		fmt.Println("EXPECTED: ", expectedAddress)
		return nil
	})

	if err != nil {
		return false, err
	}

	// compare the recovered address to the expected addr
	//
	// if they are equal, the signature is valid
	return strings.EqualFold(recoveredAddress, expectedAddress), nil
}

// []string{"tx/payload/tk"} -> string
// []string{"tx/op"} -> string
// []string{"headers/required_auths/[0]"} -> string
func convertPathMapToMessage(pathMap []struct {
	path []string
	val  interface{}
}) interface{} {
	keys := make(map[string][][]string)
	for _, pathInfo := range pathMap {
		keys[pathInfo.path[0]] = append(keys[pathInfo.path[0]], pathInfo.path[1:])
	}
	isArray := false
	for key, _ := range keys {
		isArray = isValidArr(key)
		break
	}
	if isArray {
		res := make([]interface{}, len(keys))
		for i, pathInfo := range pathMap {
			res[i] = pathInfo.val
		}
		return res
	} else {
		seenKeys := make(map[string]bool, len(keys))
		res := make(map[string]interface{}, len(keys))
		for _, pathInfo := range pathMap {
			key := pathInfo.path[0]
			if seenKeys[key] {
				continue
			}
			filteredKeys := filterMap(pathMap, key)
			if len(filteredKeys) == 1 && len(filteredKeys[0].path) == 0 {
				res[key] = filteredKeys[0].val
			} else {
				res[key] = convertPathMapToMessage(filteredKeys)
			}
			seenKeys[key] = true
		}
		return res
	}
}

// takes: pathMap, filters out all elements with path that start with provided key, removes that key from path
// pathMap, key -> new filtered and mapped pathMap

func filterMap(pathMap []struct {
	path []string
	val  interface{}
}, key string) []struct {
	path []string
	val  interface{}
} {
	newPathStructs := []struct {
		path []string
		val  interface{}
	}{}
	for _, e := range pathMap {
		if len(e.path) < 1 || e.path[0] == key {
			// starts with key -> keep
			newPathStructs = append(newPathStructs, struct {
				path []string
				val  interface{}
			}{
				path: e.path[1:],
				val:  e.val,
			})
		}
		// else, doesn't start with key, or invalid path len -> discard
	}
	return newPathStructs
}

// returns true iff array is valid
//
// examples:
// - "[]" -> false
// - "[2]" -> true
// - "4" -> false
// - "[1" -> false
// - "[two]" - false
func isValidArr(s string) bool {
	if len(s) <= 2 {
		return false
	}

	if s[0] != '[' || s[len(s)-1] != ']' {
		return false
	}

	_, err := strconv.Atoi(s[1 : len(s)-1])
	return err == nil
}

// for any list of paths like []string{"headers", "required_auths", "[0]"} will
// return true once the (0-indexed) 1st element is an array alongside its length
//
// examples:
// - []string{"headers", "required_auths", "[0]"} -> (false, 0)
// - []string{"required_auths", "[5]"} -> (true, 5)
// - []string{"required_auths", "[]"} -> (false, 0) (invalid array, also for cases like "[2" or "]")
// - []string{"[0]"} -> (false, 0)
func arrayData(strSlice []string) (bool, int) {
	if len(strSlice) < 2 {
		return false, 0
	}

	s := strSlice[1]
	if len(s) <= 2 {
		return false, 0
	}
	if s[0] != '[' || s[len(s)-1] != ']' {
		return false, 0
	}
	val, err := strconv.Atoi(s[1 : len(s)-1])
	if err != nil {
		return false, 0
	}
	return true, val
}

func ConvertCBORToEIP712TypedData(domainName string, data []byte, primaryTypeName string, floatHandler func(f float64) (*big.Int, error)) (TypedData, error) {
	reader := bytes.NewReader(data)
	decoder := cbor.NewDecoder(reader)

	pathMap := make([]struct {
		path []string
		val  interface{}
	}, 0)
	typeMap := make([]struct {
		typeName []string
		val      apitypes.Type
	}, 0)

	typedData := TypedData{}
	typedData.Data.Domain = apitypes.TypedDataDomain{Name: domainName}
	typedData.Data.PrimaryType = primaryTypeName

	v := cbor.JoinVisitors(
		cbor.Visitor{
			IntVisitor: func(path []string, val *big.Int) error {
				typeName := append([]string{primaryTypeName}, path[:len(path)-1]...)

				typeMap = append(typeMap, struct {
					typeName []string
					val      apitypes.Type
				}{
					typeName: typeName,
					val: apitypes.Type{
						Name: path[len(path)-1],
						Type: "uint256",
					},
				})
				pathMap = append(pathMap, struct {
					path []string
					val  interface{}
				}{
					path: path,
					val:  val,
				})
				return nil
			},
			NilVisitor: func(path []string) error {
				return fmt.Errorf("no nil exists")
			},
			Float32Visitor: func(path []string, val float32) error {
				v, err := floatHandler(float64(val))
				if err != nil {
					return err
				}
				typeName := append([]string{primaryTypeName}, path[:len(path)-1]...)

				typeMap = append(typeMap, struct {
					typeName []string
					val      apitypes.Type
				}{
					typeName: typeName,
					val: apitypes.Type{
						Name: path[len(path)-1],
						Type: "uint256",
					},
				})
				pathMap = append(pathMap, struct {
					path []string
					val  interface{}
				}{
					path: path,
					val:  v,
				})
				return nil
			},
			Float64Visitor: func(path []string, val float64) error {
				v, err := floatHandler(val)
				if err != nil {
					return err
				}
				typeName := append([]string{primaryTypeName}, path[:len(path)-1]...)

				typeMap = append(typeMap, struct {
					typeName []string
					val      apitypes.Type
				}{
					typeName: typeName,
					val: apitypes.Type{
						Name: path[len(path)-1],
						Type: "uint256",
					},
				})

				pathMap = append(pathMap, struct {
					path []string
					val  interface{}
				}{
					path: path,
					val:  v,
				})
				return nil
			},
			EmptyArrayVisitor: func(path []string) error {
				return nil
			},
		},
		cbor.NewBytesCollector(func(path []string, val []byte) error {
			typeName := append([]string{primaryTypeName}, path[:len(path)-1]...)

			typeMap = append(typeMap, struct {
				typeName []string
				val      apitypes.Type
			}{
				typeName: typeName,
				val: apitypes.Type{
					Name: path[len(path)-1],
					Type: "byte[]",
				},
			})
			pathMap = append(pathMap, struct {
				path []string
				val  interface{}
			}{
				path: path,
				val:  val,
			})
			return nil
		}),
		cbor.NewStringCollector(func(path []string, val string) error {
			typeName := append([]string{primaryTypeName}, path[:len(path)-1]...)
			typeMap = append(typeMap, struct {
				typeName []string
				val      apitypes.Type
			}{
				typeName: typeName,
				val: apitypes.Type{
					Name: path[len(path)-1],
					Type: "string",
				},
			})
			pathMap = append(pathMap, struct {
				path []string
				val  interface{}
			}{
				path: path,
				val:  val,
			})

			return nil
		}),
	)

	err := decoder.Decode(v)
	if err != nil {
		return TypedData{}, fmt.Errorf("failed to decode CBOR into typed data with values: %v", err)
	}

	uniqueTypes := make(map[string]uint)
	for _, types := range typeMap {
		uniqueTypes[strings.Join(types.typeName, ".")] += 1
	}

	// primaryTypeName.path.path.path : []{Name: x, Type: y}
	types := make(map[string][]apitypes.Type, len(uniqueTypes))
	for _, partialType := range typeMap {
		for i, _ := range partialType.typeName {
			before := partialType.typeName[:i+1] // [tx_container], [tx_container, tx], [tx_container, tx, payload]
			after := partialType.typeName[i+1:]
			typeName := strings.Join(before, "_")
			if len(after) == 0 {
				types[typeName] = append(types[typeName], partialType.val)
			} else {
				val, exists := types[typeName]
				if !exists || !slices.ContainsFunc(val, func(t apitypes.Type) bool {
					return t.Name == after[0]
				}) {
					isArray := isValidArr(partialType.val.Name)
					if isArray {
						typeNameToFind := append(before, after[0])
						arrayType := typeMap[slices.IndexFunc(typeMap, func(findType struct {
							typeName []string
							val      apitypes.Type
						}) bool {
							return slices.Equal(findType.typeName, typeNameToFind)
						})].val.Type
						types[typeName] = append(val, apitypes.Type{Name: after[0], Type: arrayType + "[]"})
						break
					} else {
						types[typeName] = append(val, apitypes.Type{Name: after[0], Type: typeName + "_" + after[0]})
					}
				}
			}
		}
	}

	typedData.Data.Types = types
	typedData.Data.Message = convertPathMapToMessage(pathMap).(map[string]interface{})

	err = runDebug(func() error {
		for key, tv := range typedData.Data.Message {
			fmt.Println(key, ":", tv)
		}

		jsonData, err := json.MarshalIndent(typedData, "", "  ")
		if err != nil {
			fmt.Println("Error:", err)
			return err
		}
		fmt.Println(string(jsonData))
		return nil
	})

	// jsonData, err := json.MarshalIndent(typedData, "", "  ")
	// if err != nil {
	// 	fmt.Println("Error:", err)
	// }
	// fmt.Println(string(jsonData))

	// chainId : nil
	// version: ""

	return typedData, err
}

// ===== EthProvider =====

type EthProvider struct {
	Priv *ecdsa.PrivateKey
}

func NewEthProvider(priv *ecdsa.PrivateKey) *EthProvider {

	return &EthProvider{
		Priv: priv,
	}
}

// ===== implementing the Provider interface =====

func (e *EthProvider) Sign(block blocks.Block) (string, error) {
	// todo: implement a way to sign the payload from the EthProvider

	var decodedData map[string]interface{}
	if err := decodeFromCBOR(block.RawData(), &decodedData); err != nil {
		return "", fmt.Errorf("failed to decode CBOR data: %v", err)
	}

	payload, err := ConvertCBORToEIP712TypedData("vsc.network", block.RawData(), "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})

	if err != nil {
		return "", fmt.Errorf("failed to convert block to EIP-712 typed data: %v", err)
	}

	dataHash, err := computeEIP712Hash(payload.Data)

	sig, err := ethCrypto.Sign(dataHash, e.Priv)

	fmt.Println("Hex sig", hex.EncodeToString(sig))

	return "0x" + hex.EncodeToString(sig), nil
}

// ===== utils =====

func computeEIP712Hash(typedData apitypes.TypedData) ([]byte, error) {
	// add the EIP712Domain type to the types
	typedData.Types["EIP712Domain"] = []apitypes.Type{
		{
			Name: "name",
			Type: "string",
		},
	}

	// hash the domain
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return nil, fmt.Errorf("failed to hash domain separator: %v", err)
	}

	realTypedData := typedData
	err = runDebug(func() error {
		fmt.Println("domain", hexutil.Encode(domainSeparator))

		err = mapstructure.Decode(typedData, &realTypedData)
		if err != nil {
			return err
		}

		fmt.Println("real typed data:", realTypedData)
		enc, err := realTypedData.EncodeData(typedData.PrimaryType, typedData.Message, 1)
		if err != nil {
			return err
		}
		fmt.Println("real encoded:", enc.String())
		return nil
	})
	if err != nil {
		return nil, err
	}

	// hash the message
	messageHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return nil, fmt.Errorf("failed to hash message: %v", err)
	}

	err = runDebug(func() error {
		fmt.Println("msg", typedData.PrimaryType, "\n", typedData.Message, "\n", typedData.Types, "\n", hexutil.Encode(messageHash))
		return nil
	})
	if err != nil {
		return nil, err
	}

	// combine the two hashes as per EIP-712 spec
	finalHash := ethCrypto.Keccak256(
		bytes.Join(
			[][]byte{
				{0x19, 0x01}, // EIP-712 spec prefix indicator bytes
				domainSeparator,
				messageHash,
			},
			[]byte{},
		),
	)

	err = runDebug(func() error {
		fmt.Println("final", hexutil.Encode(finalHash))
		return nil
	})
	if err != nil {
		return nil, err
	}

	return finalHash, nil
}

// decode CBOR back into a map[string]interface{}
func decodeFromCBOR(data []byte, out interface{}) error {
	// var tempData map[string]interface{}
	codec.NewDecoderBytes(data, &codec.CborHandle{}).Decode(out)
	// if err := cbor.DecodeInto(data, &tempData); err != nil {
	// 	return fmt.Errorf("failed to decode CBOR data: %v", err)
	// }

	// set the decoded data back into the output
	//
	// this is a bit hacky, but it seems to work
	// reflect.ValueOf(out).Elem().Set(reflect.ValueOf(tempData))

	return nil
}

// ===== struct/interface -> EIP-712 typed data conversion logic =====

// struct wrapper for EIP-712 typed data
type TypedData struct {
	Data apitypes.TypedData
}

type EIP712DomainType struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// this allows us to serialize the EIP-712 domain field separately outside of the types field and instead in the main obj
func (d EIP712DomainType) MarshalJSON() ([]byte, error) {
	// Structure to serialize
	domain := []map[string]string{
		{
			"name": d.Name,
			"type": d.Type,
		},
	}

	return json.Marshal(domain)
}

// marshals typed data into JSON, handling the domain field separately
func (d TypedData) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Types        apitypes.Types            `json:"types"`
		PrimaryType  string                    `json:"primaryType"`
		Domain       map[string]interface{}    `json:"domain"`
		Message      apitypes.TypedDataMessage `json:"message"`
		EIP712Domain EIP712DomainType          `json:"EIP712Domain"`
	}

	// serializes only the "name" field for the domain
	domain := make(map[string]interface{})
	if d.Data.Domain.Name != "" {
		domain["name"] = d.Data.Domain.Name
	}

	alias := Alias{
		Types:       d.Data.Types,
		PrimaryType: d.Data.PrimaryType,
		Domain:      domain,
		Message:     d.Data.Message,
		// this allows us to serialize the EIP-712 domain field separately outside of the types field and instead in the main object
		EIP712Domain: EIP712DomainType{
			Name: "name",
			Type: "string",
		},
	}

	return json.Marshal(alias)
}

func ConvertToEIP712TypedData(
	domainName string,
	data interface{},
	primaryTypeName string,
	floatHandler func(float64) (*big.Int, error),
) (TypedData, error) {

	if domainName == "" || primaryTypeName == "" {
		return TypedData{}, fmt.Errorf("domain name or primary type name cannot be empty")
	}

	// try to assert data as map[string]interface{} first
	dataMap, ok := data.(map[string]interface{})
	if !ok {
		// if not ok, try to marshal and then unmarshal the data into a map
		jsonBytes, err := json.Marshal(data)
		if err != nil {
			return TypedData{}, fmt.Errorf("failed to marshal struct: %v", err)
		}

		err = json.Unmarshal(jsonBytes, &dataMap)
		if err != nil {
			return TypedData{}, fmt.Errorf("failed to unmarshal into map: %v", err)
		}
	}

	newMap := make(map[any]interface{})
	for k, v := range data.(map[string]interface{}) {
		newMap[k] = v
	}

	// gen the msg and types
	message, types, err := generateTypedDataWithPath(newMap, primaryTypeName, floatHandler)
	if err != nil {
		return TypedData{}, fmt.Errorf("failed to generate typed data: %v", err)
	}

	// populate the typed data struct
	typedData := TypedData{}
	typedData.Data.Domain = apitypes.TypedDataDomain{Name: domainName}
	typedData.Data.PrimaryType = primaryTypeName
	typedData.Data.Message = message
	typedData.Data.Types = types

	return typedData, nil
}

// is the string an Ethereum addr?
func isEthAddr(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

// gens typed data recursively for nested maps and slices/arrays
func generateTypedDataWithPath(
	data map[any]interface{},
	typeName string,
	floatHandler func(float64) (*big.Int, error),
) (map[string]interface{}, map[string][]apitypes.Type, error) {

	message := make(map[string]interface{})
	types := make(map[string][]apitypes.Type)
	types[typeName] = []apitypes.Type{}

	// collects and sorts field names
	var fieldNames []any
	for fieldName := range data {
		fieldNames = append(fieldNames, fieldName)
	}
	// sort.Strings(fieldNames)

	for _, fieldName := range fieldNames {
		fieldValue := data[fieldName]
		fieldKind := reflect.ValueOf(fieldValue).Kind()
		var fieldType string

		switch fieldKind {
		case reflect.Slice, reflect.Array:
			// checks if the array | slice is empty
			arrayVal := reflect.ValueOf(fieldValue)
			if arrayVal.Len() == 0 {
				fieldType = "undefined[]" // allow undefined for empty arrays, as per the JS version in the Bitcoin wrapper UI
				message[fieldName.(string)] = fieldValue
			} else {
				// check the first elem to infer the inner type of the slice/array
				firstElem := arrayVal.Index(0).Interface()
				elemKind := reflect.TypeOf(firstElem).Kind()

				switch elemKind {
				case reflect.String:
					fieldType = "string[]"
					message[fieldName.(string)] = fieldValue

				case reflect.Uint, reflect.Uint16, reflect.Uint32, reflect.Uint64:
					fieldType = "uint256[]"
					uintArrayValues := make([]*big.Int, arrayVal.Len())
					for i := 0; i < arrayVal.Len(); i++ {
						uintVal := arrayVal.Index(i).Interface()
						var u64 uint64
						switch v := uintVal.(type) {
						case uint, uint8, uint16, uint32, uint64:
							u64 = v.(uint64)
						default:
							return nil, nil, fmt.Errorf("unsupported uint type in array for field %s", fieldName)
						}
						uintArrayValues[i] = new(big.Int).SetUint64(u64)
					}
					message[fieldName.(string)] = uintArrayValues

				case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
					fieldType = "int256[]"
					intArrayValues := make([]*big.Int, arrayVal.Len())
					for i := 0; i < arrayVal.Len(); i++ {
						intVal := arrayVal.Index(i).Interface()
						var i64 int64
						switch v := intVal.(type) {
						case int, int8, int16, int32, int64:
							i64 = reflect.ValueOf(v).Int()
						default:
							return nil, nil, fmt.Errorf("unsupported int type in array for field %s", fieldName)
						}
						intArrayValues[i] = big.NewInt(i64)
					}
					message[fieldName.(string)] = intArrayValues

				case reflect.Float64:
					fieldType = "uint256[]"
					bigIntArray := make([]*big.Int, arrayVal.Len())
					for i := 0; i < arrayVal.Len(); i++ {
						floatVal := arrayVal.Index(i).Interface()
						if f64, ok := floatVal.(float64); ok {
							bigInt, err := floatHandler(f64)
							if err != nil {
								return nil, nil, fmt.Errorf("failed to handle float array value: %v", err)
							}
							bigIntArray[i] = bigInt
						} else {
							return nil, nil, fmt.Errorf("invalid float value in array")
						}
					}
					message[fieldName.(string)] = bigIntArray

				case reflect.Uint8:
					// treat []uint8 as bytes
					fieldType = "bytes"
					message[fieldName.(string)] = fieldValue

				default:
					// fallback/default for unrecognized slice types
					fieldType = fmt.Sprintf("%s[]", elemKind.String())
					message[fieldName.(string)] = fieldValue
				}
			}

		case reflect.Map:
			// nested maps gen new type names and processes recursively
			fmt.Println("VAL: ", reflect.ValueOf(fieldValue).Kind())
			nestedTypeName := fmt.Sprintf("%s.%s", typeName, fieldName)
			fmt.Println("pre-NESTED: ", fieldValue)
			nestedData, ok := fieldValue.(map[any]interface{})
			if !ok {
				fmt.Println(nestedTypeName, fieldValue)
				fmt.Println(reflect.ValueOf(fieldValue).MapKeys()[0].Interface())
				return nil, nil, fmt.Errorf("379 expected map[string]interface{} for field '%s'", fieldName)
			}
			fmt.Println("NESTED: ", nestedData)
			nestedMessage, nestedTypes, err := generateTypedDataWithPath(nestedData, nestedTypeName, floatHandler)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to generate typed data for nested map: %v", err)
			}
			fieldType = nestedTypeName
			message[fieldName.(string)] = nestedMessage
			for k, v := range nestedTypes {
				types[k] = v
			}

		case reflect.String:
			// handle eth addr or regular strings
			//
			// if the string is an Ethereum address, use the "address" type
			if isEthAddr(fieldValue.(string)) {
				fieldType = "address"
			} else {
				fieldType = "string"
			}
			message[fieldName.(string)] = fieldValue

		case reflect.Float64:
			// use the float handler for all float values
			if floatValue, ok := fieldValue.(float64); ok {
				bigIntValue, err := floatHandler(floatValue)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to handle float value: %v", err)
				}
				message[fieldName.(string)] = bigIntValue
				fieldType = "uint256"
			} else {
				return nil, nil, fmt.Errorf("expected float64 for field '%s'", fieldName)
			}

		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			// convert all integers to big int using type assertions
			var i64 int64
			switch v := fieldValue.(type) {
			case int, int8, int16, int32, int64:
				i64 = reflect.ValueOf(v).Int()
			default:
				return nil, nil, fmt.Errorf("unsupported integer type for field '%s'", fieldName)
			}
			message[fieldName.(string)] = big.NewInt(i64)
			fieldType = "int256"

		case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
			// convert all integers to big int using type assertions
			var u64 uint64
			switch v := fieldValue.(type) {
			case uint, uint8, uint16, uint32, uint64:
				u64 = v.(uint64)
			default:
				return nil, nil, fmt.Errorf("unsupported unsigned integer type for field '%s'", fieldName)
			}
			message[fieldName.(string)] = new(big.Int).SetUint64(u64)
			fieldType = "uint256"

		case reflect.Bool:
			fieldType = "bool"
			message[fieldName.(string)] = fieldValue

		default:
			return nil, nil, fmt.Errorf("unsupported field type %s for field %s", fieldKind.String(), fieldName)
		}

		// append field and its type to the types array
		types[typeName] = append(types[typeName], apitypes.Type{Name: fieldName.(string), Type: fieldType})
	}

	// sort the `types[typeName]` slice to ensure consistent ordering
	//
	// this is primarily for deterministic EIP-712 hash generation, else, tests "sometimes" pass
	sort.Slice(types[typeName], func(i, j int) bool {
		return types[typeName][i].Name < types[typeName][j].Name
	})

	return message, types, nil
}
