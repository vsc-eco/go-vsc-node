package dids

import (
	"encoding/hex"
	"fmt"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// ===== interface assertions =====

// ethr addr | payload type
var _ DID[string, apitypes.TypedData] = EthDID("")

var _ Provider = &EthProvider{}
var _ EthDIDProvider = &EthProvider{}

// ===== EthDID =====

type EthDID string

func NewEthDID(ethAddr string) EthDID {
	return EthDID("did:ethr:" + ethAddr)
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
	return string(d)[9:]
}

func (d EthDID) Verify(payload apitypes.TypedData, sig string) (bool, error) {
	dataHash, err := ComputeEIP712Hash(payload)
	if err != nil {
		return false, fmt.Errorf("failed to compute EIP712 hash: %v", err)
	}

	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return false, fmt.Errorf("failed to decode signature: %v", err)
	}

	// Adjust V value back to [0, 1] if necessary
	// todo: add docs
	if sigBytes[64] != 0 && sigBytes[64] != 1 {
		sigBytes[64] -= 27
	}

	address := d.Identifier()

	if len(dataHash) != 32 {
		return false, fmt.Errorf("invalid data hash length: expected 32 bytes, got %d", len(dataHash))
	}

	if len(sigBytes) != 65 {
		return false, fmt.Errorf("invalid signature length: expected 65 bytes, got %d", len(sigBytes))
	}

	pubKey, err := crypto.SigToPub(dataHash, sigBytes)
	if err != nil {
		return false, fmt.Errorf("failed to recover public key from signature: %v", err)
	}

	recoveredAddr := crypto.PubkeyToAddress(*pubKey).Hex()
	return recoveredAddr == address, nil
}

// ===== EthProvider =====

type EthProvider struct {
	// todo: in the future, we may want to store some sort of priv key here (future-proofing)
}

func NewEthProvider() *EthProvider {
	return &EthProvider{}
}

// ===== implementing the Provider and EthDIDProvider interfaces =====

func (e *EthProvider) Sign(payload map[string]interface{}) (string, error) {
	// todo: implement a way to sign the payload from the EthProvider
	panic("unimplemented")
}

func (e *EthProvider) RecoverSigner(payload apitypes.TypedData, sig string) (string, error) {
	dataHash, err := ComputeEIP712Hash(payload)
	if err != nil {
		return "", fmt.Errorf("failed to compute EIP712 hash: %v", err)
	}

	sigBytes, err := hex.DecodeString(sig)
	if err != nil {
		return "", fmt.Errorf("failed to decode signature: %v", err)
	}

	if len(sigBytes) != 65 {
		return "", fmt.Errorf("invalid signature length: expected 65 bytes, got %d", len(sigBytes))
	}

	// adjust V value back to [0, 1] for recovery if necessary
	if sigBytes[64] != 0 && sigBytes[64] != 1 {
		sigBytes[64] -= 27
	}

	// recover the pub key
	pubKey, err := crypto.SigToPub(dataHash, sigBytes)
	if err != nil {
		return "", fmt.Errorf("failed to recover public key from signature: %v", err)
	}

	// return the recovered addr
	address := crypto.PubkeyToAddress(*pubKey).Hex()
	return address, nil
}

// ===== utils =====

// computes the EIP-712 hash for the provided typed data
func ComputeEIP712Hash(typedData apitypes.TypedData) ([]byte, error) {
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
