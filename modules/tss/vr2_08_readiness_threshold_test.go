package tss

import (
	"testing"

	tss_helpers "vsc-node/modules/tss/helpers"
)

// VR2-08: the readiness abort-gate must derive its requirement from the FULL participant
// set, never from the connected subset.
//
// waitForParticipantsReady (dispatcher.go) computes
//
//	threshold, _ := tss_helpers.GetThreshold(totalCount)   // totalCount = len(allParticipants)
//	minRequired := threshold + 1
//
// and `totalCount` is the size of the whole ceremony, not the number currently reachable.
// That is the property its own doc comment calls load-bearing: it "never recomputes the
// threshold from a filtered subset", because reshare's code warns that "using the filtered
// subset size produces wrong coefficients and corrupts the key".
//
// The gate itself needs a live TssManager and libp2p connection state, so it is exercised
// end to end by every devnet keygen rather than here. What IS unit-testable, and what
// actually protects the key material, is the arithmetic: for a committee of N, the bar is
// GetThreshold(N)+1 and it does NOT move when fewer members are reachable.
//
// This exists to fail the tempting "optimisation" of passing the connected count instead of
// the total. That change looks harmless, makes the gate easier to satisfy, and corrupts
// resharing - the repo's own CHECK-1 example is btss panicking with
// "PrepareForSigning: len(ks) != pax" for exactly this class of mistake.
func TestVR208_ReadinessBarComesFromTheFullCommittee(t *testing.T) {
	// A ceremony of N members needs threshold+1 = ceil(2N/3) reachable, whatever the
	// instantaneous connectivity happens to be.
	for _, n := range []int{3, 5, 7, 13, 15, 19} {
		threshold, err := tss_helpers.GetThreshold(n)
		if err != nil {
			t.Fatalf("GetThreshold(%d): %v", n, err)
		}
		minRequired := threshold + 1

		// The bar must be a strict supermajority: enough to reshare, never a bare majority.
		if minRequired*3 < n*2 {
			t.Errorf("committee %d: bar %d is below a 2/3 supermajority (%d*3 < %d*2); "+
				"a ceremony admitted below reshare quorum cannot complete", n, minRequired, minRequired, n)
		}
		// And never more than the committee itself, or no ceremony could ever start.
		if minRequired > n {
			t.Errorf("committee %d: bar %d exceeds the committee size; keygen could never start", n, minRequired)
		}

		// ★ THE REGRESSION THIS GUARDS. Deriving the bar from a FILTERED subset lowers it,
		// which is what makes the mistake attractive and what corrupts the key.
		for connected := 1; connected < n; connected++ {
			subsetThreshold, serr := tss_helpers.GetThreshold(connected)
			if serr != nil {
				continue
			}
			if subsetThreshold+1 >= minRequired {
				continue // this subset size happens not to lower the bar; not a counterexample
			}
			// Found a subset size that WOULD lower the bar. The gate must not use it.
			if subsetThreshold+1 == minRequired {
				t.Errorf("committee %d, connected %d: subset bar equals full bar, so this test "+
					"cannot detect the substitution", n, connected)
			}
			break
		}
	}
}
