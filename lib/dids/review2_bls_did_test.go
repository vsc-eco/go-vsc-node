package dids_test

import (
	"testing"

	"vsc-node/lib/dids"
)

// review2 MEDIUM #100/#101 — BlsDID.Identifier() sliced
// string(d)[len(prefix):], data[2:] and (*[48]byte)(...) with no length
// guards, so a malformed DID (from untrusted on-chain election keys / tss
// commitments) panicked (slice bounds out of range) in block/commitment
// processing; callers then also nil-deref'd the result.
//
// Differential: #170 baseline panics on these inputs (RED); fix/review2
// returns nil safely (GREEN). A valid DID still resolves.
func TestReview2BlsDIDIdentifierNoPanic(t *testing.T) {
	bad := []string{
		"",
		"x",
		"did:",
		"did:key:",
		"did:key:z",
		"did:key:zzzzzzzz",
		"not-a-did",
	}
	for _, in := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("review2 #100: BlsDID(%q).Identifier() panicked: %v", in, r)
				}
			}()
			if pk := dids.BlsDID(in).Identifier(); pk != nil {
				t.Fatalf("BlsDID(%q).Identifier() = %v, want nil for malformed DID", in, pk)
			}
		}()
	}
}
