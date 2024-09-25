package eip712

import (
	"crypto/ecdsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"
)

var (
	ErrSerialization        = errors.New("serialization error")
	ErrDeserialization      = errors.New("deserialization error")
	ErrHashing              = errors.New("hashing error")
	ErrSigning              = errors.New("signing error")
	ErrSigIncorrectLen      = errors.New("signature incorrect length")
	ErrRecoveringPubKey     = errors.New("failed to recover public key")
	ErrInvalidType          = errors.New("invalid type")
	ErrNameAndTypeZeroValue = errors.New("name and type must not be zero value")
)

// tested on: https://regex101.com/
var fixedArrayRegex = regexp.MustCompile(`^([a-zA-Z0-9]+)\[(\d+)\]$`)

var validTypes = make(map[string]bool)

// called on package init
func init() {
	setValidTypes()
}

func setValidTypes() {
	for i := 1; i <= 32; i++ {
		validTypes[fmt.Sprintf("bytes%d", i)] = true
	}
	for _, t := range []string{"uint8", "uint16", "uint32", "uint64", "uint128", "uint256", "int8", "int16", "int32", "int64", "int128", "int256", "bool", "address", "bytes", "string"} {
		validTypes[t] = true
	}

	for _, t := range []string{"uint256", "address", "bytes", "string"} {
		validTypes[fmt.Sprintf("%s[]", t)] = true
	}
}

// all these types adhere roughly to the type defs here from vsc and shared gist:
// - https://github.com/vsc-eco/client/blob/483a3f7bc182a75785fb8e1118868bfe6e60db97/src/utils.ts#L152
// - https://gist.github.com/APTy/f2a6864a97889793c587635b562c7d72

// domain of the message
//
// abides by: https://eips.ethereum.org/EIPS/eip-712, adapted for Go
// they list to cite this as: Remco Bloemen (@Recmo), Leonid Logvinov (@LogvinovLeon), Jacob Evans (@dekz), "EIP-712: Typed structured data hashing and signing," Ethereum Improvement Proposals, no. 712, September 2017. [Online serial]. Available: https://eips.ethereum.org/EIPS/eip-712.
type EIP712Domain struct {
	// Eth expects uint256 int: https://ethereum.stackexchange.com/questions/143114/can-chainid-be-overflow
	ChainId           uint256.Int `json:"chainId"`
	Name              string      `json:"name"`
	VerifyingContract string      `json:"verifyingContract"`
	Version           string      `json:"version"`
}

func (d *EIP712Domain) DomainSeparator() string {
	domainDef := "EIP712Domain(string name,string version,uint256 chainId,address verifyingContract)"

	// hash the domain type definition
	typeHash := sha3.NewLegacyKeccak256()
	typeHash.Write([]byte(domainDef))
	typeHashSum := typeHash.Sum(nil)

	// hash the name field
	nameHash := sha3.NewLegacyKeccak256()
	nameHash.Write([]byte(d.Name))
	nameHashSum := nameHash.Sum(nil)

	// hash the version field
	versionHash := sha3.NewLegacyKeccak256()
	versionHash.Write([]byte(d.Version))
	versionHashSum := versionHash.Sum(nil)

	//  32-byte array for chainID
	chainIdBytes := make([]byte, 32)
	chainId := d.ChainId.Bytes()
	// place the chainID bytes into the last remaining n (32 - len(chainID)) bytes of the 32-byte array
	copy(chainIdBytes[32-len(chainId):], chainId)
	// then hash this value
	chainIdHash := sha3.NewLegacyKeccak256()
	chainIdHash.Write(chainIdBytes)
	chainIdHashSum := chainIdHash.Sum(nil)

	// hash the verifyingContract directly as a 20-byte address padded to 32 bytes
	//
	// since this is 20 bytes, we pad the first 12 bytes, then copy the address bytes after this
	verifyingContractHash := sha3.NewLegacyKeccak256()
	verifyingContractBytes := make([]byte, 32)
	copy(verifyingContractBytes[12:], []byte(d.VerifyingContract))
	verifyingContractHash.Write(verifyingContractBytes)
	verifyingContractHashSum := verifyingContractHash.Sum(nil)

	// aggregate everything into the final domain separator hash
	finalHash := sha3.NewLegacyKeccak256()
	finalHash.Write(typeHashSum)
	finalHash.Write(nameHashSum)
	finalHash.Write(versionHashSum)
	finalHash.Write(chainIdHashSum)
	finalHash.Write(verifyingContractHashSum)

	// return as hex
	return fmt.Sprintf("0x%x", finalHash.Sum(nil))
}

// overall message to be signed
type TypedData struct {
	Domain      EIP712Domain               `json:"domain"`
	PrimaryType string                     `json:"primaryType"`
	Message     map[string][]FieldWithType `json:"message"`
	Types       map[string][]FieldWithType `json:"types"`
}

// defines the fields of the typed data
type FieldWithType struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// ensures all types used within this struct's Message and Types fields are valid based on: https://eips.ethereum.org/EIPS/eip-712
func (td *TypedData) ValidateTypes() error {
	// aggregate fields
	var combinedFields []FieldWithType
	for _, f := range td.Types {
		combinedFields = append(combinedFields, f...)
	}
	for _, f := range td.Message {
		combinedFields = append(combinedFields, f...)
	}

	for _, f := range combinedFields {
		// ensure all fields have a name and type
		if f.Name == "" || f.Type == "" {
			return ErrNameAndTypeZeroValue
		}

		// check if the type is valid
		if !isValidType(f.Type) {
			return ErrInvalidType
		}
	}
	return nil
}

// checks whether a type is valid, including arrays and fixed-size arrays
func isValidType(fieldType string) bool {
	// easy case: directly matches
	if validTypes[fieldType] {
		return true
	}

	// else, we need to check for dynamic/fixed-size arrays of valid types
	// like: uint256[], uint256[2], address[], etc.
	if fixedArrayRegex.MatchString(fieldType) {
		parts := fixedArrayRegex.FindStringSubmatch(fieldType)
		baseType := parts[1] // get the "base", like "address[]" -> "address"
		// we need this "base" to be an already valid type
		return validTypes[baseType]
	}

	// else, invalid
	return false
}

// hashes the typed data according to eip712 rules
func HashTypedData(typedData TypedData) ([]byte, error) {
	// validate types
	if err := typedData.ValidateTypes(); err != nil {
		return nil, err
	}

	// compute the domain separator
	domainSeparator := typedData.Domain.DomainSeparator()

	// serialize the message part of the typed data structure
	data, err := json.Marshal(typedData.Message)
	if err != nil {
		return nil, ErrSerialization
	}

	// hash the message data using keccak-256
	messageHash := sha3.NewLegacyKeccak256()
	_, err = messageHash.Write(data)
	if err != nil {
		return nil, ErrHashing
	}

	// create a combined hash for domain separator and message hash
	// as per the specification's specific order
	finalHash := sha3.NewLegacyKeccak256()
	finalHash.Write([]byte("\x19\x01")) // eip191 specifying separator, as outlined: https://eips.ethereum.org/EIPS/eip-712
	finalHash.Write([]byte(domainSeparator))
	finalHash.Write(messageHash.Sum(nil))

	// return the final combined hash
	return finalHash.Sum(nil), nil
}

// signs the eip712 typed data using secp256k1 priv key
//
// ref: https://github.com/w3c-ccg/ethereum-eip712-signature-2021-spec/blob/main/README.md
func SignTypedData(typedData TypedData, privKey *ecdsa.PrivateKey) (string, error) {

	if err := typedData.ValidateTypes(); err != nil {
		return "", err
	}

	// the typed data hashed using our predefined fn
	hashOfTypedData, err := HashTypedData(typedData)
	if err != nil {
		return "", err
	}

	// sign our hash with the priv key
	signature, err := crypto.Sign(hashOfTypedData, privKey)
	if err != nil {
		return "", ErrSigning
	}

	// return the sig base64 encoded
	return base64.StdEncoding.EncodeToString(signature), nil
}

// takes an addr, typedData, and sigBase64, and returns if the sig is valid
func IsValidSignature(typedData TypedData, sigBase64, addr string) (bool, error) {
	// recover the signer's addr from the sig
	signerAddr, err := RecoverAddrOfSigner(typedData, sigBase64)
	if err != nil {
		return false, err
	}

	// compare the recovered addr to the one provided
	return signerAddr == addr, nil
}

// recovers the ethereum addr from the base64-encoded sig and typed data struct
func RecoverAddrOfSigner(typedData TypedData, sigBase64 string) (string, error) {

	// validate types
	if err := typedData.ValidateTypes(); err != nil {
		return "", err
	}

	// base64 -> sig byte slice
	sig, err := base64.StdEncoding.DecodeString(sigBase64)
	if err != nil {
		return "", fmt.Errorf("failed to decode signature: %v", err)
	}

	// if sig len wrong, then we need to return an error
	if len(sig) != 32+32+1 { // 1 byte for recovery ID and 64 for the sig (32 + 32), ref: https://forum.dfinity.org/t/t-ecdsa-can-someone-explain-v-65-bytes-vs-64-byte-signatures/28118/5
		return "", ErrSigIncorrectLen
	}

	// adjust the standard ethereum recovery ID byte (v) from 27/28 to 0/1 by
	// removing 27 from it UNLESS it's already 0 or 1
	//
	// this alters domain of the 65th byte from {27/28} to {0/1}
	if sig[64] != 0 && sig[64] != 1 {
		sig[64] -= 27
	}

	// re-hash typed data
	hash, err := HashTypedData(typedData)
	if err != nil {
		return "", err
	}

	// recover the pub key from the sig using the hash
	pubKey, err := crypto.Ecrecover(hash, sig)
	if err != nil {
		return "", ErrRecoveringPubKey
	}

	// convert pub key to an ethereum addr
	pubECDSAKey, err := crypto.UnmarshalPubkey(pubKey)
	// normal error check and also checking if this pub key is nil, not sure if that can
	// happen, but better safe than sorry
	if err != nil || pubECDSAKey == nil {
		return "", ErrDeserialization
	}

	// convert to hex
	return crypto.PubkeyToAddress(*pubECDSAKey).Hex(), nil
}
