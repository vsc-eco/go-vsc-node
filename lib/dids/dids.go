package dids

import (
	"github.com/ipfs/go-cid"
)

// ===== DIDs =====

type DID[T any] interface {
	String() string
	Identifier() T
	Verify(cid cid.Cid, sig string) (bool, error)
}

// ===== provider interface (can be passed around later, depending on how DIDs want to be used) =====

type Provider interface {
	Sign(cid cid.Cid) (string, error)
}
