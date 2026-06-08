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

		// Fresh election: 2/3.
		require.Equalf(t, twoThirds, MinimalRequiredElectionVotes(0, n), "fresh N=%d", n)

		// Fully stale (2 weeks): must NOT have decayed below 2/3.
		require.Equalf(t, twoThirds, MinimalRequiredElectionVotes(MAX_BLOCKS_SINCE_LAST_ELECTION, n),
			"fully-stale N=%d must stay at the 2/3 quorum, not N/2+1", n)

		// Halfway through the decay window: still at/above 2/3.
		require.GreaterOrEqualf(t, MinimalRequiredElectionVotes(MAX_BLOCKS_SINCE_LAST_ELECTION/2, n), twoThirds,
			"mid-decay N=%d must stay >= 2/3", n)
	}
}
