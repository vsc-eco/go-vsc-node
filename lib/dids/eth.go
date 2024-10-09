package dids

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"sort"
	"strings"

	cbor "github.com/ipfs/go-ipld-cbor"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
	blocks "github.com/ipfs/go-block-format"
)

// ===== constants =====

// matches what is from vsc's system:
// - https://github.com/vsc-eco/Bitcoin-wrap-UI/blob/365d24bc592003be9600f8a0c886e4e6f9bbb1c1/src/hooks/auth/wagmi-web3modal/index.ts#L10
// - https://github.com/w3c-ccg/did-pkh/blob/main/did-pkh-method-draft.md
const EthDIDPrefix = "did:pkh:eip155:1:"

// ===== interface assertions =====

// ethr addr | payload type
var _ DID[string] = EthDID("")

var _ Provider = &EthProvider{}

// ===== EthDID =====

type EthDID string

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
	// decode the block using CBOR into a generic type of map[string]interface
	var decodedData map[string]interface{}
	if err := decodeFromCBOR(block.RawData(), &decodedData); err != nil {
		return false, fmt.Errorf("failed to decode CBOR data: %v", err)
	}

	// convert the sorted decoded data into EIP-712 typed data
	payload, err := ConvertToEIP712TypedData("vsc.network", decodedData, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to convert block to EIP-712 typed data: %v", err)
	}

	// compute the EIP-712 hash
	dataHash, err := computeEIP712Hash(payload.Data)
	if err != nil {
		return false, fmt.Errorf("failed to compute EIP-712 hash: %v", err)
	}

	// decode the sig from the hex
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %v", err)
	}

	// recover the pub key from the signature and data hash
	pubKey, err := crypto.SigToPub(dataHash, sigBytes)
	if err != nil {
		return false, fmt.Errorf("failed to recover public key from signature: %v", err)
	}

	// extract the recovered addr
	recoveredAddress := crypto.PubkeyToAddress(*pubKey).Hex()

	// get the expected addr from the DID
	expectedAddress := d.Identifier()

	// compare the recovered address to the expected addr
	//
	// if they are equal, the signature is valid
	return strings.EqualFold(recoveredAddress, expectedAddress), nil
}

// ===== EthProvider =====

type EthProvider struct {
	// todo: in the future, we may want to store some sort of priv key here (future-proofing)
}

func NewEthProvider() *EthProvider {
	return &EthProvider{}
}

// ===== implementing the Provider interface =====

func (e *EthProvider) Sign(block blocks.Block) (string, error) {
	// todo: implement a way to sign the payload from the EthProvider
	panic("unimplemented")
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

	// hash the message
	messageHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return nil, fmt.Errorf("failed to hash message: %v", err)
	}

	// combine the two hashes as per EIP-712 spec
	finalHash := crypto.Keccak256(
		[]byte("\x19\x01"), // EIP-712 spec prefix indicator bytes
		domainSeparator,
		messageHash,
	)

	return finalHash, nil
}

// decode CBOR back into a map[string]interface{}
func decodeFromCBOR(data []byte, out interface{}) error {
	var tempData map[string]interface{}
	if err := cbor.DecodeInto(data, &tempData); err != nil {
		return fmt.Errorf("failed to decode CBOR data: %v", err)
	}

	// set the decoded data back into the output
	//
	// this is a bit hacky, but it seems to work
	reflect.ValueOf(out).Elem().Set(reflect.ValueOf(tempData))

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

	// gen the msg and types
	message, types, err := generateTypedDataWithPath(dataMap, primaryTypeName, floatHandler)
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
	data map[string]interface{},
	typeName string,
	floatHandler func(float64) (*big.Int, error),
) (map[string]interface{}, map[string][]apitypes.Type, error) {

	message := make(map[string]interface{})
	types := make(map[string][]apitypes.Type)
	types[typeName] = []apitypes.Type{}

	// collects and sorts field names
	var fieldNames []string
	for fieldName := range data {
		fieldNames = append(fieldNames, fieldName)
	}
	sort.Strings(fieldNames)

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
				message[fieldName] = fieldValue
			} else {
				// check the first elem to infer the inner type of the slice/array
				firstElem := arrayVal.Index(0).Interface()
				elemKind := reflect.TypeOf(firstElem).Kind()

				switch elemKind {
				case reflect.String:
					fieldType = "string[]"
					message[fieldName] = fieldValue

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
					message[fieldName] = uintArrayValues

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
					message[fieldName] = intArrayValues

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
					message[fieldName] = bigIntArray

				case reflect.Uint8:
					// treat []uint8 as bytes
					fieldType = "bytes"
					message[fieldName] = fieldValue

				default:
					// fallback/default for unrecognized slice types
					fieldType = fmt.Sprintf("%s[]", elemKind.String())
					message[fieldName] = fieldValue
				}
			}

		case reflect.Map:
			// nested maps gen new type names and processes recursively
			nestedTypeName := fmt.Sprintf("%s.%s", typeName, fieldName)
			nestedData, ok := fieldValue.(map[string]interface{})
			if !ok {
				return nil, nil, fmt.Errorf("expected map[string]interface{} for field '%s'", fieldName)
			}
			nestedMessage, nestedTypes, err := generateTypedDataWithPath(nestedData, nestedTypeName, floatHandler)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to generate typed data for nested map: %v", err)
			}
			fieldType = nestedTypeName
			message[fieldName] = nestedMessage
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
			message[fieldName] = fieldValue

		case reflect.Float64:
			// use the float handler for all float values
			if floatValue, ok := fieldValue.(float64); ok {
				bigIntValue, err := floatHandler(floatValue)
				if err != nil {
					return nil, nil, fmt.Errorf("failed to handle float value: %v", err)
				}
				message[fieldName] = bigIntValue
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
			message[fieldName] = big.NewInt(i64)
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
			message[fieldName] = new(big.Int).SetUint64(u64)
			fieldType = "uint256"

		case reflect.Bool:
			fieldType = "bool"
			message[fieldName] = fieldValue

		default:
			return nil, nil, fmt.Errorf("unsupported field type %s for field %s", fieldKind.String(), fieldName)
		}

		// append field and its type to the types array
		types[typeName] = append(types[typeName], apitypes.Type{Name: fieldName, Type: fieldType})
	}

	// sort the `types[typeName]` slice to ensure consistent ordering
	//
	// this is primarily for deterministic EIP-712 hash generation, else, tests "sometimes" pass
	sort.Slice(types[typeName], func(i, j int) bool {
		return types[typeName][i].Name < types[typeName][j].Name
	})

	return message, types, nil
}
