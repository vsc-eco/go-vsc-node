package oracle

import (
	"vsc-node/lib/intmath"
)

// FeedTickSnapshotInt is the consensus-grade integer mirror of FeedTickSnapshot.
//
// Float math in TrustedHivePrice + MovingAverageRing iterates a Go map and
// accumulates float64s, which is non-deterministic across nodes. The integer
// snapshot here re-derives the price aggregate by sorting trusted witnesses
// and summing SQ64 quotes — bit-equal across nodes given the same on-chain
// inputs. This is the only form persisted to consensus state by W2.
//
// Per-witness reward-reduction bps are computed by the rewards/ package
// from L2 evidence (vsc_blocks + tss_commitments) at snapshot persistence
// time and stored alongside this snapshot — they are NOT carried on the
// in-memory tracker.
type FeedTickSnapshotInt struct {
	TickBlockHeight uint64

	// SQ64 fixed-point: HBD per HIVE.
	TrustedHiveMean intmath.SQ64
	TrustedHiveOK   bool

	HiveMovingAvg   intmath.SQ64
	HiveMovingAvgOK bool

	HBDInterestRateBps int
	HBDInterestRateOK  bool

	TrustedWitnessGroup []string
}

// LastTickInt returns the most recent tick as a deterministic integer snapshot.
//
// The trusted HIVE mean is computed at TickIfDue time from rational Quote
// pairs (HbdRaw / HiveRaw) iterated in sorted witness order — bit-equal across
// nodes given the same on-chain inputs. We just read the cached SQ64 result
// here; no recomputation needed.
//
// HiveMovingAvg is the SQ64 form of the float MA. Now that its float inputs
// are themselves derived deterministically from SQ64 means, the MA value is
// also deterministic across nodes — but it remains marked informational and
// is slated for a native-SQ64 (or post-SQ64) rewrite alongside the wider
// pendulum integer migration.
func (t *FeedTracker) LastTickInt() FeedTickSnapshotInt {
	if t == nil {
		return FeedTickSnapshotInt{}
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	out := FeedTickSnapshotInt{
		TickBlockHeight:    t.last.TickBlockHeight,
		HBDInterestRateBps: t.last.HBDInterestRateBps,
		HBDInterestRateOK:  t.last.HBDInterestRateOK,
		HiveMovingAvg:      intmath.SQ64FromFloat(t.last.HiveMovingAvg),
		HiveMovingAvgOK:    t.last.HiveMovingAvgOK,
		TrustedHiveMean:    t.lastMeanSQ64,
		TrustedHiveOK:      t.lastMeanSQ64OK,
	}

	if len(t.last.TrustedWitnessGroup) > 0 {
		out.TrustedWitnessGroup = append([]string(nil), t.last.TrustedWitnessGroup...)
	}

	return out
}

