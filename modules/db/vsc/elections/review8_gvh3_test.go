package elections

// review8 GV-H3 — the election ACCEPT threshold decayed from 2/3 down to a bare
// majority (N/2+1) over 2 weeks, so a patient minority controlling >N/2 (vs the
// 2/3 needed when fresh) could wait out the timer and finalize a captured
// committee. The fix floors the threshold at the BFT-safe 2/3 quorum.

import (
	"math"
	"testing"

	"vsc-node/modules/common/consensusversion"

	"github.com/stretchr/testify/require"
)

func TestReview8_GVH3_ThresholdNeverBelowTwoThirds(t *testing.T) {
	v020 := consensusversion.V0_2_0
	for _, n := range []uint64{7, 20, 40} {
		twoThirds := uint64(math.Ceil(float64(n) * 2.0 / 3.0))

		// Threshold at 0.2.0+ is the fixed 2/3 quorum (no time-based decay).
		require.Equalf(t, twoThirds, MinimalRequiredElectionVotes(0, n, v020),
			"N=%d must be the 2/3 quorum, not N/2+1", n)
	}

	// Pre-0.2.0: fully stale threshold drops to bare majority.
	v010 := consensusversion.Version{Major: 0, Consensus: 1, NonConsensus: 0}
	n := uint64(20)
	bareMajority := uint64(math.Floor(float64(n)/2 + 1)) // 11
	staleVal := MinimalRequiredElectionVotes(MAX_BLOCKS_SINCE_LAST_ELECTION, n, v010)
	require.Equalf(t, bareMajority, staleVal,
		"pre-0.2.0 fully-stale N=%d must be bare majority floor(N/2+1)=%d", n, bareMajority)
}
