package elections

// review8 GV-H3 — the election ACCEPT threshold decayed from 2/3 down to a bare
// majority (N/2+1) over 2 weeks, so a patient minority controlling >N/2 (vs the
// 2/3 needed when fresh) could wait out the timer and finalize a captured
// committee. The fix floors the threshold at the BFT-safe 2/3 quorum.

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReview8_GVH3_ThresholdNeverBelowTwoThirds(t *testing.T) {
	for _, n := range []uint64{7, 20, 40} {
		twoThirds := uint64(math.Ceil(float64(n) * 2.0 / 3.0))

		// Threshold is the fixed 2/3 quorum (no time-based decay).
		require.Equalf(t, twoThirds, MinimalRequiredElectionVotes(n),
			"N=%d must be the 2/3 quorum, not N/2+1", n)
	}
}
