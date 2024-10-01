package dids

import (
	"encoding/hex"
	"fmt"

	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// ===== constants =====

// matching the "did:ethr" part from:
// - https://github.com/decentralized-identity/ethr-did-resolver/blob/master/doc/did-method-spec.md
// - https://github.com/uport-project/ethr-did

// could be: "did:pkh:eip155:1:" as that matches what is from vsc's system (however, both seem to be valid):
// - https://github.com/vsc-eco/Bitcoin-wrap-UI/blob/365d24bc592003be9600f8a0c886e4e6f9bbb1c1/src/hooks/auth/wagmi-web3modal/index.ts#L10
const EthDIDPrefix = "did:ethr:"

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
	recoveredDID, err := d.RecoverSigner(payload, sig)
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

func (d EthDID) RecoverSigner(payload apitypes.TypedData, sig string) (DID[string, apitypes.TypedData], error) {
	// compute the EIP-712 hash
	dataHash, err := ComputeEIP712Hash(payload)
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
func ComputeEIP712Hash(typedData apitypes.TypedData) ([]byte, error) {
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
