package dids

import (
	"crypto/ed25519"

	"github.com/ethereum/go-ethereum/signer/core/apitypes"
)

// ===== DIDs =====

type DID[T any, V any] interface {
	String() string
	Identifier() T
	Verify(payload V, sig string) (bool, error)
}

// ===== interfaces (can be passed around later, depending on how DIDs want to be used) =====

type Provider interface {
	Sign(payload map[string]interface{}) (string, error)
}

type KeyDIDProvider interface {
	Provider
	CreateJWE(payload map[string]interface{}, to ed25519.PublicKey) (string, error)
	DecryptJWE(jwe string) (map[string]interface{}, error)
}

// future-proofing by adding this interface
type EthDIDProvider interface {
	Provider
	// todo: move this up to the DID interface?
	RecoverSigner(payload apitypes.TypedData, sig string) (string, error)
}
