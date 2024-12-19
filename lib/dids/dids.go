package dids

// ===== DIDs =====

type DID[T any, V any] interface {
	String() string
	Identifier() T
	Verify(data V, sig string) (bool, error)
}

// ===== provider interface (can be passed around later, depending on how DIDs want to be used) =====

type Provider[V any] interface {
	Sign(data V) (string, error)
}
