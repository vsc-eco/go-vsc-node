package dids

import blocks "github.com/ipfs/go-block-format"

// ===== DIDs =====

type DID[T any] interface {
	String() string
	Identifier() T
	Verify(block blocks.Block, sig string) (bool, error)
}

// ===== provider interface (can be passed around later, depending on how DIDs want to be used) =====

type Provider interface {
	Sign(block blocks.Block) (string, error)
}
