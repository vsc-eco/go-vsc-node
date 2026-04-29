package oracle

import (
	"math/big"
	"sort"

	"vsc-node/lib/intmath"
)

// FeedTickSnapshotInt is the consensus-grade integer mirror of FeedTickSnapshot.
//
// Float math in TrustedHivePrice + MovingAverageRing iterates a Go map and
// accumulates float64s, which is non-deterministic across nodes. The integer
// snapshot here re-derives the price aggregate by sorting trusted witnesses
// and summing SQ64 quotes — bit-equal across nodes given the same on-chain
// inputs. This is the only form persisted to consensus state by W2.
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

	// WitnessSlashBps stored as a sorted slice (NOT a map) so the persisted
	// bson form is byte-deterministic across nodes.
	WitnessSlashBps []WitnessSlashEntry
}

// WitnessSlashEntry is one (witness → bps) row in a snapshot's slash list.
type WitnessSlashEntry struct {
	Witness string
	Bps     int
}

// LastTickInt returns the most recent tick as a deterministic integer snapshot.
//
// Recomputes the trusted HIVE price from the per-witness quote map by:
//  1. Sorting trusted witnesses by name (deterministic order across nodes).
//  2. Converting each quote to SQ64 (bit-exact float→int rounding).
//  3. Summing in big.Int and dividing by count via integer arithmetic.
//
// The HiveMovingAvg field is the SQ64 form of the float MA, which is itself
// non-deterministic across nodes when its inputs were non-deterministic floats.
// This is acceptable for the v1 testnet: only TrustedHiveMean feeds the
// pendulum settlement math; HiveMovingAvg is informational.
// TODO(W2 follow-up): replace MovingAverageRing with an SQ64 ring once the
// tracker is migrated end-to-end.
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
	}

	if len(t.last.TrustedWitnessGroup) > 0 {
		out.TrustedWitnessGroup = append([]string(nil), t.last.TrustedWitnessGroup...)
	}

	out.TrustedHiveMean, out.TrustedHiveOK = t.deterministicTrustedMeanLocked()

	out.WitnessSlashBps = sortedSlashEntries(t.last.WitnessSlashBps)
	return out
}

// deterministicTrustedMeanLocked recomputes the trusted-witness HIVE mean as SQ64.
// Caller must hold t.mu. Iterates trusted witnesses in lexicographic order so the
// SQ64 sum (and the resulting mean after integer division) is identical across nodes.
func (t *FeedTracker) deterministicTrustedMeanLocked() (intmath.SQ64, bool) {
	trusted := t.last.TrustedWitnessGroup
	if len(trusted) == 0 || len(t.quotes) == 0 {
		return 0, false
	}

	names := append([]string(nil), trusted...)
	sort.Strings(names)

	sum := new(big.Int)
	count := 0
	for _, w := range names {
		q, ok := t.quotes[w]
		if !ok || q <= 0 {
			continue
		}
		sum.Add(sum, big.NewInt(int64(intmath.SQ64FromFloat(q))))
		count++
	}
	if count == 0 {
		return 0, false
	}
	mean := new(big.Int).Quo(sum, big.NewInt(int64(count)))
	return intmath.SQ64(mean.Int64()), true
}

func sortedSlashEntries(m map[string]int) []WitnessSlashEntry {
	if len(m) == 0 {
		return nil
	}
	out := make([]WitnessSlashEntry, 0, len(m))
	for w, bps := range m {
		out = append(out, WitnessSlashEntry{Witness: w, Bps: bps})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Witness < out[j].Witness })
	return out
}
