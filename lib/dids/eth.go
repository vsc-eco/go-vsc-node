package dids

import (
	"encoding/hex"
	"fmt"
	"reflect"
	"strings"

	"github.com/ethereum/go-ethereum/common/math"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// ===== constants =====

// matches what is from vsc's system:
// - https://github.com/vsc-eco/Bitcoin-wrap-UI/blob/365d24bc592003be9600f8a0c886e4e6f9bbb1c1/src/hooks/auth/wagmi-web3modal/index.ts#L10
const EthDIDPrefix = "did:pkh:eip155:1:"

// ===== interface assertions =====

// ethr addr | payload type
var _ DID[string, apitypes.TypedData] = EthDID("")

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

func (d EthDID) Verify(payload apitypes.TypedData, sig string) (bool, error) {
	// internally use RecoverSigner func to recover the signer DID from the sig
	recoveredDID, err := d.recoverSigner(payload, sig)
	if err != nil {
		return false, fmt.Errorf("failed to recover signer: %v", err)
	}

	// match the recovered DID's identifier with the current DID's identifier
	return recoveredDID.Identifier() == d.Identifier(), nil
}

// ===== EthProvider =====

type EthProvider struct {
	// todo: in the future, we may want to store some sort of priv key here (future-proofing)
}

func NewEthProvider() *EthProvider {
	return &EthProvider{}
}

// ===== implementing the Provider interface =====

func (e *EthProvider) Sign(payload map[string]interface{}) (string, error) {
	// todo: implement a way to sign the payload from the EthProvider
	panic("unimplemented")
}

func (d EthDID) recoverSigner(payload apitypes.TypedData, sig string) (DID[string, apitypes.TypedData], error) {
	// compute the EIP-712 hash
	dataHash, err := computeEIP712Hash(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to compute EIP712 hash: %v", err)
	}

	// decode the sig from hex
	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %v", err)
	}

	// ensure the sig is 65 bytes long
	if len(sigBytes) != 65 {
		return nil, fmt.Errorf("invalid signature length: expected 65 bytes, got %d", len(sigBytes))
	}

	// ensure the data hash is 32 bytes long
	if len(dataHash) != 32 {
		return nil, fmt.Errorf("invalid data hash length: expected 32 bytes, got %d", len(dataHash))
	}

	// adjust V value back to [0, 1] if necessary
	//
	// this Gist was provided in the team Notion: https://gist.github.com/APTy/f2a6864a97889793c587635b562c7d72
	// it demos the need to subtract 27 from the 65th (index 64) byte of the sig
	//
	// internally, this Gist says it does this for this reason: https://github.com/ethereum/go-ethereum/blob/55599ee95d4151a2502465e0afc7c47bd1acba77/internal/ethapi/api.go#L442
	//
	// this is also described on the official ethereum site in an article
	// by Vitalik Buterin: https://eips.ethereum.org/EIPS/eip-155
	// which describes this in EIP-155
	if sigBytes[64] != 0 && sigBytes[64] != 1 {
		sigBytes[64] -= 27
	}

	// recover the pub key
	pubKey, err := crypto.SigToPub(dataHash, sigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to recover public key from signature: %v", err)
	}

	// return the recovered addr
	address := crypto.PubkeyToAddress(*pubKey).Hex()
	return NewEthDID(address), nil
}

// ===== utils =====

// computes the EIP-712 hash for the provided typed data
func computeEIP712Hash(typedData apitypes.TypedData) ([]byte, error) {
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return nil, fmt.Errorf("failed to hash domain separator: %v", err)
	}

	// hash the message
	messageHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return nil, fmt.Errorf("failed to hash message: %v", err)
	}

	// hash the final hash in the EIP-712 format as described here: https://eips.ethereum.org/EIPS/eip-712#Specification
	finalHash := crypto.Keccak256(
		[]byte("\x19\x01"),
		domainSeparator,
		messageHash,
	)
	return finalHash, nil
}

// ===== struct -> EIP-712 typed data conversion logic =====

// converts a go struct to eip-712 typed data and validates it
func ConvertToEIP712TypedData(
	domainName, version, verifyingContract, salt string,
	chainId *math.HexOrDecimal256, data interface{}, primaryTypeName string,
	floatHandler func(float64) (*math.HexOrDecimal256, error)) (apitypes.TypedData, error) {

	// convert go struct to eip-712 format
	message, types, err := goStructsToTypedData(data, primaryTypeName, make(map[string]bool), make(map[string][]apitypes.Type), floatHandler)
	if err != nil {
		return apitypes.TypedData{}, err
	}

	// add the EIP712Domain type
	types["EIP712Domain"] = []apitypes.Type{
		{Name: "name", Type: "string"},
		{Name: "version", Type: "string"},
		{Name: "chainId", Type: "uint256"},
		{Name: "verifyingContract", Type: "address"},
		{Name: "salt", Type: "bytes32"},
	}

	// construct the full eip-712 typed data
	typedData := apitypes.TypedData{
		Domain: apitypes.TypedDataDomain{
			Name:              domainName,
			Version:           version,
			ChainId:           chainId,
			VerifyingContract: verifyingContract,
			Salt:              salt,
		},
		PrimaryType: primaryTypeName,
		Message:     message,
		Types:       types,
	}

	// validate the final typed data
	if err := validateTypedData(typedData); err != nil {
		return apitypes.TypedData{}, err
	}

	return typedData, nil
}

// converts go types to eip-712 typed data recursively
func goStructsToTypedData(
	data interface{},
	typeName string,
	addedTypes map[string]bool,
	typeDefs map[string][]apitypes.Type,
	floatHandler func(float64) (*math.HexOrDecimal256, error)) (apitypes.TypedDataMessage, apitypes.Types, error) {

	typedData := apitypes.TypedDataMessage{}
	types := apitypes.Types{}

	v := reflect.ValueOf(data)
	t := reflect.TypeOf(data)

	// check for nil or non-struct values
	if v.Kind() != reflect.Struct {
		return nil, nil, fmt.Errorf("unsupported kind: %s", t.Kind().String())
	}

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fieldValue := v.Field(i)

		// skip nil pointer fields
		if fieldValue.Kind() == reflect.Ptr && fieldValue.IsNil() {
			continue
		}

		fieldName := strings.ToLower(field.Name)
		fieldType := getEIP712Type(field.Type)

		switch fieldValue.Kind() {
		case reflect.Struct:
			// recurse into structs
			subTypedData, subTypes, err := goStructsToTypedData(fieldValue.Interface(), fieldType, addedTypes, typeDefs, floatHandler)
			if err != nil {
				return nil, nil, err
			}
			typedData[fieldName] = subTypedData
			if !addedTypes[field.Type.Name()] {
				types[field.Type.Name()] = subTypes[field.Type.Name()]
				addedTypes[field.Type.Name()] = true
			}
		case reflect.Array, reflect.Slice:
			// ensure it's a supported slice type
			if !isValidEIP712Array(fieldValue.Type()) {
				return nil, nil, fmt.Errorf("unsupported array/slice type: %s", fieldValue.Type())
			}

			// handle byte slices (bytes[] or fixed bytesN)
			if fieldValue.Type().Elem().Kind() == reflect.Uint8 {
				if fieldValue.Len() == 0 {
					// empty byte array, set to "0x"
					typedData[fieldName] = "0x"
				} else {
					// handle dynamic byte arrays (bytes[]), like [65]byte
					bytes := make([]byte, fieldValue.Len())
					for j := 0; j < fieldValue.Len(); j++ {
						bytes[j] = byte(fieldValue.Index(j).Uint())
					}
					// serialize as dynamic bytes[]
					typedData[fieldName] = fmt.Sprintf("0x%x", bytes)
				}
			} else {
				arrayData, err := handleArray(fieldValue, fieldType, floatHandler)
				if err != nil {
					return nil, nil, err
				}
				typedData[fieldName] = arrayData
			}
		default:
			// handle primitive types
			primitiveValue, err := handlePrimitive(fieldValue.Interface(), fieldType, floatHandler)
			if err != nil {
				return nil, nil, err
			}
			typedData[fieldName] = primitiveValue
		}

		// add the field's type definition
		typeDefs[typeName] = append(typeDefs[typeName], apitypes.Type{Name: fieldName, Type: fieldType})
	}

	types[typeName] = typeDefs[typeName]
	return typedData, types, nil
}

// returns the EIP-712 type for a go type
func getEIP712Type(t reflect.Type) string {
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "bool"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return "int256"
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "uint256"
	case reflect.Float32, reflect.Float64:
		return "uint256"
	case reflect.Array, reflect.Slice:
		if t.Elem().Kind() == reflect.Uint8 {
			if t.Len() > 32 {
				return "bytes" // coerce byte arrays > 32 bytes into dynamic bytes[] else apitypes from go-ethereum will error
			}
			return fmt.Sprintf("bytes%d", t.Len()) // handle fixed-length byte arrays as bytesN as is prevalent in the go-ethereum keys file
		}
		return fmt.Sprintf("%s[]", getEIP712Type(t.Elem())) // dynamic arrays with elem type then []
	case reflect.Struct:
		return t.Name() // else, just take the struct name
	default:
		return "object" // unsupported types we just consider "object"
	}
}

// checks if a slice or array is a valid EIP-712 array, coercing integer types
func isValidEIP712Array(t reflect.Type) bool {
	switch t.Elem().Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.String, reflect.Bool:
		return true
	default:
		return false
	}
}

// handles arrays and slices, preserving int[], uint[], string[], bool[], address[]
func handleArray(v reflect.Value, fieldType string, floatHandler func(float64) (*math.HexOrDecimal256, error)) ([]interface{}, error) {
	sliceLen := v.Len()
	sliceData := make([]interface{}, sliceLen)
	for j := 0; j < sliceLen; j++ {
		elem := v.Index(j)
		primitiveValue, err := handlePrimitive(elem.Interface(), fieldType, floatHandler)
		if err != nil {
			return nil, err
		}
		sliceData[j] = primitiveValue
	}
	return sliceData, nil
}

// handles primitive types like int, string, bool, etc
func handlePrimitive(value interface{}, fieldType string, floatHandler func(float64) (*math.HexOrDecimal256, error)) (interface{}, error) {
	switch v := value.(type) {
	case string, bool:
		return v, nil
	case int, int8, int16, int32, int64:
		// convert integer types to hex format
		return fmt.Sprintf("0x%x", v), nil
	case uint, uint8, uint16, uint32, uint64:
		// convert unsigned integer types to hex format
		return fmt.Sprintf("0x%x", v), nil
	case float32, float64:
		// handle floats using the provided float handler
		return floatHandler(v.(float64))
	default:
		// if type is unsupported, return an error
		return nil, fmt.Errorf("unsupported field type: %s", fieldType)
	}
}

// validates the EIP-712 typed data
func validateTypedData(typedData apitypes.TypedData) error {
	// ensure we don't have any "object" types as they are unsupported
	for typeName, fields := range typedData.Types {
		for _, field := range fields {
			if field.Type == "object" {
				return fmt.Errorf("unsupported field type 'object' in type %s for field %s", typeName, field.Name)
			}
		}
	}

	// attempt to hash the typed data for validation
	//
	// we don't care what the result is, only if it errors out as
	// the official ethereum/go-ethereum lib has tons of data validation
	_, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		return fmt.Errorf("failed to hash typed data: %v", err)
	}

	return nil
}
