package oracle

import (
	"math/big"
	"sort"
)

// Tiny helper file: the geometry computer uses big.Int locally for overflow
// detection but every persisted field is int64. Keeping these in one place
// makes it obvious where the int64↔big.Int boundary lives.

func bigMul(a, b int64) *big.Int {
	out := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return out
}

func bigQuo(a *big.Int, b int64) *big.Int {
	if b == 0 {
		return new(big.Int)
	}
	out := new(big.Int).Quo(a, big.NewInt(b))
	return out
}

func sortStrings(s []string) {
	sort.Strings(s)
}
