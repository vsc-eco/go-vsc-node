package dids

import (
	"encoding/hex"
	"math/big"
	"testing"

	"github.com/decred/dcrd/dcrec/secp256k1/v2"
)

// A gateway-key PoP with R == 0 (or R >= N) and a valid low S must be refused
// with an error. secp256k1/v2 v2.0.1 RecoverCompact does not range-check R and
// nil-panics on R == 0, and the PoP comes from a witness announcement anyone
// can publish; the election proposer verifies every enabled witness's PoP on
// every election, so a panic here takes the node down at every election.
func TestVerifyGatewayKeyPoP_RejectsOutOfRangeR(t *testing.T) {
	n := secp256k1.S256().Params().N
	for name, r := range map[string]*big.Int{"zero": big.NewInt(0), "N": new(big.Int).Set(n), "N+1": new(big.Int).Add(n, big.NewInt(1))} {
		for hdr := byte(27); hdr <= 34; hdr++ {
			sig := make([]byte, gatewayPoPSigSize)
			sig[0] = hdr
			if r.Sign() > 0 {
				r.FillBytes(sig[1:33])
			}
			sig[64] = 1 // S = 1: low and non-zero, so only R is wrong
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("R=%s hdr=%d: VerifyGatewayKeyPoP panicked: %v", name, hdr, p)
					}
				}()
				if err := VerifyGatewayKeyPoP("STM5abc", "alice", hex.EncodeToString(sig)); err == nil {
					t.Fatalf("R=%s hdr=%d: accepted", name, hdr)
				}
			}()
		}
	}
}
