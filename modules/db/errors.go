package db

import "errors"

// ErrDecode marks a stored document that was read but did not decode into its
// type. It is a property of the stored bytes, so every node holding the same
// row gets the same error. Consensus readers can decide on it, where a driver
// or network error must be retried.
var ErrDecode = errors.New("stored document failed to decode")
