package rewards

import "sort"

// EpochInputsProvider abstracts the per-tick L2 evidence the aggregator
// consumes for epoch-wide reduction computation. The state engine implements
// this from on-chain reads (vsc_blocks, tss_commitments, schedule); tests can
// provide a stub.
//
// Implementations MUST be deterministic functions of on-chain data so two
// honest nodes computing reductions for the same epoch produce byte-equal
// per-witness totals.
type EpochInputsProvider interface {
	// TickInputsForRange returns the TickInputs the aggregator should score
	// for the tick at `tickHeight`. The committee is the elected set active
	// at `tickHeight`. Returns false if the tick can't be scored (e.g.,
	// missing election lookup) — the caller skips that tick.
	TickInputsForRange(tickHeight uint64) (TickInputs, bool)
}

// ComputeReductionsForEpoch derives per-witness consolidated reduction bps
// for the closed epoch by re-running per-tick scoring across `(fromBh, toBh]`
// directly from on-chain L2 evidence. Skips the local snapshot store entirely
// — the result is deterministic across nodes given the same chain state.
//
// The returned map omits witnesses with zero effective bps after applying
// PerEpochForgivenessBps and PerEpochCapBps, matching AggregateEpoch's
// existing snapshot-driven contract.
func ComputeReductionsForEpoch(
	provider EpochInputsProvider,
	fromBh, toBh uint64,
	tickInterval uint64,
) map[string]int {
	if provider == nil || tickInterval == 0 || toBh <= fromBh {
		return nil
	}

	// First tick height in (fromBh, toBh]: smallest multiple of tickInterval
	// that is strictly greater than fromBh.
	firstTick := fromBh + tickInterval - (fromBh % tickInterval)

	totals := make(map[string]int)
	for tickHeight := firstTick; tickHeight <= toBh; tickHeight += tickInterval {
		in, ok := provider.TickInputsForRange(tickHeight)
		if !ok {
			continue
		}
		records := AggregateTick(in)
		for _, r := range records {
			if r.Bps <= 0 {
				continue
			}
			totals[r.Witness] += r.Bps
		}
	}
	if len(totals) == 0 {
		return nil
	}

	out := make(map[string]int, len(totals))
	for w, raw := range totals {
		eff := raw - PerEpochForgivenessBps
		if eff <= 0 {
			continue
		}
		if eff > PerEpochCapBps {
			eff = PerEpochCapBps
		}
		out[w] = eff
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// SortedAccountsFromMap returns the keys of a reductions map in
// lexicographic order — a re-export shim for callers building deterministic
// settlement payloads.
func SortedAccountsFromMap(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
