package dids

import (
	"crypto/ed25519"
)

// ===== DIDs =====

type DID[T any, V any] interface {
	String() string
	Identifier() T
	Verify(payload V, sig string) (bool, error)
	RecoverSigner(payload V, sig string) (DID[T, V], error)
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
