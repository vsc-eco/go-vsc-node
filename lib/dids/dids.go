package dids

import (
	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// ===== DIDs =====

type DID interface {
	FullString() string
}

// ===== interfaces (can be passed around later, depending on how DIDs want to be used) =====

type Provider interface {
	DID() DID
	Sign(payload interface{}) (string, error)
}

type KeyDIDProvider interface {
	Provider
	CreateJWE(payload map[string]interface{}, to DID) (string, error)
	DecryptJWE(jwe string) (map[string]interface{}, error)
}

type EthDIDProvider interface {
	Provider
	RecoverSigner(typedData apitypes.TypedData, sig string) (DID, error)
}
