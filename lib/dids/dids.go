package dids

// ===== DIDs =====

type DID[T any, V any] interface {
	String() string
	Identifier() T
	Verify(payload V, sig string) (bool, error)
}

// ===== provider interface (can be passed around later, depending on how DIDs want to be used) =====

type Provider interface {
	Sign(payload map[string]interface{}) (string, error)
}
