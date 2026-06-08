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

// EpochReductionEvidence is the per-account explanation behind the single
// consolidated reduction bps that lands in the settlement record. Signals
// carries each liveness signal's bps summed across the epoch's ticks;
// RawConsolidatedBps is the sum of the per-tick max-of — the pre-forgiveness,
// pre-cap total ComputeReductionsForEpoch accumulates before subtracting
// PerEpochForgivenessBps.
//
// The Signals fields do NOT add up to the consolidated reduction: the
// consolidated path takes the per-tick max across signals before summing
// across ticks, so they are contributing evidence, not additive shares.
//
// Derived purely from on-chain L2 evidence at fixed snapshot heights — two
// honest nodes produce identical maps — but it is NOT part of the signed
// settlement payload. It backs a local explorer/diagnostic view the state
// engine persists at settlement-apply time.
type EpochReductionEvidence struct {
	Signals            WitnessLivenessEvidence
	RawConsolidatedBps int
}

// ComputeReductionEvidenceForEpoch re-runs per-tick scoring across
// `(fromBh, toBh]` and accumulates, per witness, the per-signal bps breakdown
// plus the raw consolidated total. It shares the exact tick loop, provider,
// and skip logic of ComputeReductionsForEpoch so the breakdown lines up with
// the consolidated reductions that drive settlement; the only difference is
// what it keeps from each AggregateTick record.
//
// Witnesses that accrued no penalty (every tick scored 0) are omitted, so the
// map holds only accounts that registered at least one signal during the epoch.
func ComputeReductionEvidenceForEpoch(
	provider EpochInputsProvider,
	fromBh, toBh uint64,
	tickInterval uint64,
) map[string]EpochReductionEvidence {
	if provider == nil || tickInterval == 0 || toBh <= fromBh {
		return nil
	}

	firstTick := fromBh + tickInterval - (fromBh % tickInterval)

	acc := make(map[string]EpochReductionEvidence)
	for tickHeight := firstTick; tickHeight <= toBh; tickHeight += tickInterval {
		in, ok := provider.TickInputsForRange(tickHeight)
		if !ok {
			continue
		}
		for _, r := range AggregateTick(in) {
			if r.Bps <= 0 {
				continue
			}
			e := acc[r.Witness]
			e.Signals.BlockProductionBps += r.Evidence.BlockProductionBps
			e.Signals.BlockAttestationBps += r.Evidence.BlockAttestationBps
			e.Signals.TssReshareExclusionBps += r.Evidence.TssReshareExclusionBps
			e.Signals.TssBlameBps += r.Evidence.TssBlameBps
			e.Signals.TssSignNonParticipationBps += r.Evidence.TssSignNonParticipationBps
			e.Signals.OracleQuoteDivergenceBps += r.Evidence.OracleQuoteDivergenceBps
			e.RawConsolidatedBps += r.Bps
			acc[r.Witness] = e
		}
	}
	if len(acc) == 0 {
		return nil
	}
	return acc
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
