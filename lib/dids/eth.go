package dids

import (
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

type EthDID string

// creates a new EthDID of the form did:ethr:<address>
func NewEthDID(pubKey *ecdsa.PublicKey) EthDID {
	address := crypto.PubkeyToAddress(*pubKey).Hex()
	return EthDID("did:ethr:" + address)
}

// returns the DID as a string, mainly just for implementing the DID interface
func (d EthDID) FullString() string {
	return string(d)
}

// extracts the eth addr from the did:ethr:<address> format
func AddressFromEthDID(did string) (string, error) {
	if !strings.HasPrefix(did, "did:ethr:") {
		return "", fmt.Errorf("invalid DID format: missing did:ethr:... prefix")
	}
	address := did[9:] // remove "did:ethr:" prefix
	if !strings.HasPrefix(address, "0x") || len(address) != 42 {
		return "", fmt.Errorf("invalid eth address format")
	}
	return address, nil
}

type EthProvider struct {
	privKey *ecdsa.PrivateKey
	pubKey  *ecdsa.PublicKey
	did     EthDID
}

// creates a new eth provider using a provided ECDSA priv key
func NewEthProvider(privKey *ecdsa.PrivateKey) (*EthProvider, error) {
	if privKey == nil {
		return nil, fmt.Errorf("private key is nil")
	}
	pubKey := privKey.Public().(*ecdsa.PublicKey)
	did := NewEthDID(pubKey)

	return &EthProvider{
		privKey: privKey,
		pubKey:  pubKey,
		did:     did,
	}, nil
}

func (e *EthProvider) DID() DID {
	return e.did
}

// signs the provided eip712 TYPED data
func (e *EthProvider) Sign(payload interface{}) (string, error) {
	typedData, ok := payload.(apitypes.TypedData)
	if !ok {
		return "", fmt.Errorf("payload is not of type apitypes.TypedData")
	}

	dataToSign, err := computeEIP712Hash(typedData)
	if err != nil {
		return "", fmt.Errorf("failed to compute eip712 hash: %v", err)
	}

	sig, err := crypto.Sign(dataToSign, e.privKey)
	if err != nil {
		return "", fmt.Errorf("failed to sign data: %v", err)
	}

	// ensure v value is in [27, 28]
	if sig[64] < 27 {
		sig[64] += 27
	}

	return hex.EncodeToString(sig), nil
}

// verifies the provided signature against the typed data
func (e *EthProvider) VerifyTypedData(typedData apitypes.TypedData, sig string) (bool, error) {
	dataHash, err := computeEIP712Hash(typedData)
	if err != nil {
		return false, fmt.Errorf("failed to compute EIP712 hash: %v", err)
	}

	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %v", err)
	}

	// adjust V value back to [0, 1] for verification if necessary
	if sigBytes[64] >= 27 {
		sigBytes[64] -= 27
	}

	address, err := AddressFromEthDID(e.did.FullString())
	if err != nil {
		return false, fmt.Errorf("failed to extract Ethereum address from DID: %v", err)
	}

	valid, err := VerifySignature(address, dataHash, sigBytes)
	return valid, err
}

// recovers the signer of the given signature for the typed data
func (e *EthProvider) RecoverSigner(typedData apitypes.TypedData, sig string) (DID, error) {
	dataHash, err := computeEIP712Hash(typedData)
	if err != nil {
		return nil, fmt.Errorf("failed to compute EIP712 hash: %v", err)
	}

	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return nil, fmt.Errorf("failed to decode signature: %v", err)
	}

	if len(sigBytes) != 65 {
		return nil, fmt.Errorf("invalid signature length: expected 65 bytes, got %d", len(sigBytes))
	}

	// adjust V value back to [0, 1] for recovery if necessary
	if sigBytes[64] >= 27 {
		sigBytes[64] -= 27
	}

	pubKeyRecovered, err := crypto.SigToPub(dataHash, sigBytes)
	if err != nil {
		return nil, fmt.Errorf("failed to recover public key from signature: %v", err)
	}

	return NewEthDID(pubKeyRecovered), nil
}

// computes the eip712 hash for a given typed data
func computeEIP712Hash(typedData apitypes.TypedData) ([]byte, error) {
	domainSeparator, err := typedData.HashStruct("EIP712Domain", typedData.Domain.Map())
	if err != nil {
		return nil, fmt.Errorf("failed to hash domain separator: %v", err)
	}

	messageHash, err := typedData.HashStruct(typedData.PrimaryType, typedData.Message)
	if err != nil {
		return nil, fmt.Errorf("failed to hash message: %v", err)
	}

	finalHash := crypto.Keccak256(
		[]byte("\x19\x01"),
		domainSeparator,
		messageHash,
	)
	return finalHash, nil
}

// checks if the sig was made by the address from the DID
func VerifySignature(address string, dataHash []byte, sig []byte) (bool, error) {

	if len(dataHash) != 32 {
		return false, fmt.Errorf("invalid data hash length: expected 32 bytes, got %d", len(dataHash))
	}

	pubKey, err := crypto.SigToPub(dataHash, sig)
	if err != nil {
		return false, fmt.Errorf("failed to recover public key from signature: %v", err)
	}
	recoveredAddress := crypto.PubkeyToAddress(*pubKey).Hex()

	return strings.EqualFold(recoveredAddress, address), nil
}
