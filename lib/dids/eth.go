package dids

import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strings"

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
	// internally use recoverSigner func to recover the signer DID from the sig

	// convert the block to a typed data struct
	payload, err := ConvertToEIP712TypedData("vsc.network", block, "tx_container_v0", func(f float64) (*big.Int, error) {
		return big.NewInt(int64(f)), nil
	})
	if err != nil {
		return false, fmt.Errorf("failed to convert block to typed data: %v", err)
	}

	recoveredDID, err := d.recoverSigner(payload.Data, sig)
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

func (e *EthProvider) Sign(block blocks.Block) (string, error) {
	// todo: implement a way to sign the payload from the EthProvider
	panic("unimplemented")
}

func (d EthDID) recoverSigner(payload apitypes.TypedData, sig string) (DID[string], error) {
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

// struct wrapper for EIP-712 typed data
//
// needed, else we can't marshal the domain field separately
type TypedData struct {
	Data apitypes.TypedData
}

// marshals typed data into JSON, handling the domain field separately
//
// this is needed because the domain field is a map[string]interface{} and we only want to serialize the "name" field, if
// we don't handle this separately, the entire domain map will be serialized, including zero values and such
func (d TypedData) MarshalJSON() ([]byte, error) {
	type Alias struct {
		Types       apitypes.Types            `json:"types"`
		PrimaryType string                    `json:"primaryType"`
		Domain      map[string]interface{}    `json:"domain"`
		Message     apitypes.TypedDataMessage `json:"message"`
	}

	// serialize only the "name" field for the domain
	domain := make(map[string]interface{})
	if d.Data.Domain.Name != "" {
		domain["name"] = d.Data.Domain.Name
	}

	alias := Alias{
		Types:       d.Data.Types,
		PrimaryType: d.Data.PrimaryType,
		Domain:      domain,
		Message:     d.Data.Message,
	}

	return json.Marshal(alias)
}

func ConvertToEIP712TypedData(
	domainName string,
	data interface{}, primaryTypeName string,
	floatHandler func(float64) (*big.Int, error)) (TypedData, error) {

	// try to assert data as map[string]interface{} first
	dataMap, ok := data.(map[string]interface{})
	if !ok {
		// if not, try to marshal and then unmarshal the data into a map
		jsonBytes, err := json.Marshal(data)
		if err != nil {
			return TypedData{}, fmt.Errorf("failed to marshal struct: %v", err)
		}

		err = json.Unmarshal(jsonBytes, &dataMap)
		if err != nil {
			return TypedData{}, fmt.Errorf("failed to unmarshal into map: %v", err)
		}
	}

	// gen the message and types
	message, types, err := generateTypedData(dataMap, primaryTypeName, floatHandler)
	if err != nil {
		return TypedData{}, fmt.Errorf("failed to generate typed data: %v", err)
	}

	// add EIP712Domain type with "name" field
	types["EIP712Domain"] = []apitypes.Type{
		{Name: "name", Type: "string"},
		// no need to add more fields per vsc-eco's system
	}

	// populate the TypedData struct
	typedData := TypedData{}
	typedData.Data.Domain = apitypes.TypedDataDomain{Name: domainName}
	typedData.Data.PrimaryType = primaryTypeName
	typedData.Data.Message = message
	typedData.Data.Types = types

	// validate the final typed data
	//
	// this is the final validation check that is done by ethereum/go-ethereum's signer package; if this
	// passes, it means our type is "valid" by their standards
	if err := validateTypedData(typedData.Data); err != nil {
		return TypedData{}, err
	}

	return typedData, nil
}

// check if a string is an eth addr
func isEthAddr(s string) bool {
	if len(s) != 42 || !strings.HasPrefix(s, "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

func generateTypedData(data map[string]interface{}, typeName string, floatHandler func(float64) (*big.Int, error)) (map[string]interface{}, map[string][]apitypes.Type, error) {
	message := make(map[string]interface{})
	types := make(map[string][]apitypes.Type)
	types[typeName] = []apitypes.Type{}

	for fieldName, fieldValue := range data {
		fieldKind := reflect.ValueOf(fieldValue).Kind()
		var fieldType string

		switch fieldKind {
		case reflect.Slice, reflect.Array:
			elemType := reflect.TypeOf(fieldValue).Elem()

			// default to int256[] for arrays of integers
			if elemType.Kind() == reflect.Int || elemType.Kind() == reflect.Int8 || elemType.Kind() == reflect.Int16 || elemType.Kind() == reflect.Int32 || elemType.Kind() == reflect.Int64 {
				fieldType = "int256[]"
				// convert each int in the array to string
				intArray := reflect.ValueOf(fieldValue)
				strArray := make([]string, intArray.Len())
				for i := 0; i < intArray.Len(); i++ {
					strArray[i] = fmt.Sprintf("%d", intArray.Index(i).Int())
				}
				message[fieldName] = strArray
			} else if elemType.Kind() == reflect.Uint8 {
				fieldType = "bytes" // handle byte slices
				message[fieldName] = fieldValue
			} else {
				fieldType = fmt.Sprintf("%s[]", elemType.Kind()) // handle other slice/array types
				message[fieldName] = fieldValue
			}

		case reflect.Map:
			// handle nested maps recursively, we name them as "<FieldName>MapInterface" since we don't have the type name
			nestedTypeName := fmt.Sprintf("%sMapInterface", fieldName)
			nestedMessage, nestedTypes, err := generateTypedData(fieldValue.(map[string]interface{}), nestedTypeName, floatHandler)
			if err != nil {
				return nil, nil, fmt.Errorf("failed to generate typed data for nested map: %v", err)
			}
			fieldType = nestedTypeName
			message[fieldName] = nestedMessage
			for k, v := range nestedTypes {
				types[k] = v
			}

		case reflect.String:
			// check if the string is a valid eth addr
			if isEthAddr(fieldValue.(string)) {
				fieldType = "address"
			} else {
				fieldType = "string"
			}
			message[fieldName] = fieldValue

		case reflect.Float64:
			// handle floats using the provided float handler
			if floatValue, err := floatHandler(fieldValue.(float64)); err == nil {
				message[fieldName] = floatValue
				fieldType = "uint256"
			} else {
				return nil, nil, fmt.Errorf("failed to handle float value: %v", err)
			}

		case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
			// handle all integer types, convert to int256 and serialize as string
			intValue := reflect.ValueOf(fieldValue).Int()
			fieldType = "int256"
			message[fieldName] = fmt.Sprintf("%d", intValue)

		default:
			// handle other primitive types
			fieldType = fieldKind.String()
			message[fieldName] = fieldValue
		}

		// append the field to the types array
		types[typeName] = append(types[typeName], apitypes.Type{Name: fieldName, Type: fieldType})
	}

	return message, types, nil
}

// validates the EIP-712 typed data
//
// uses the ethereum/go-ethereum signer package to validate the typed data,
// we don't actually care about the result, we just want to know if the typed data is valid
// because the signer package will throw an error if the typed data is invalid and their validation
// logic is more comprehensive than what we could write since it's the official ethereum package
func validateTypedData(typedData apitypes.TypedData) error {
	for typeName, fields := range typedData.Types {
		for _, field := range fields {
			if field.Type == "object" {
				return fmt.Errorf("unsupported field type 'object' in type %s for field %s", typeName, field.Name)
			}
		}
	}

	_, _, err := apitypes.TypedDataAndHash(typedData)
	if err != nil {
		return fmt.Errorf("failed to hash typed data: %v", err)
	}

	return nil
}
