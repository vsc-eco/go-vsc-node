package rewards

import (
	"sort"
)

// WitnessRewardReductionRecord is one row of the per-tick reward-reduction
// list. Bps is the consolidated per-tick value (post max-of, post-clamp)
// used by settlement math. Evidence breaks out the per-signal raw bps for
// debugging and dispute resolution; the consolidated Bps is the max-of (not
// the sum), so settlement reads only Bps.
type WitnessRewardReductionRecord struct {
	Witness  string
	Bps      int
	Evidence WitnessLivenessEvidence
}

// WitnessLivenessEvidence breaks the per-tick reduction into its 5
// underlying signals. Read by `AggregateTick` callers (currently only
// `ComputeReductionsForEpoch` for settlement composition); not consumed by
// the settlement math itself.
type WitnessLivenessEvidence struct {
	BlockProductionBps         int
	BlockAttestationBps        int
	TssReshareExclusionBps     int
	TssBlameBps                int
	TssSignNonParticipationBps int
}

// TickInputs bundles the per-signal pre-fetched data the aggregator consumes
// to produce one tick's reward-reduction records. Each field is the output of
// a query the state-engine wire-in performed for the tick window
// (current_tick_block - DefaultTickIntervalBlocks, current_tick_block].
//
// Committee is the full elected committee at the tick height; every member
// gets a record in the output (even if their bps is 0).
type TickInputs struct {
	Committee []string

	Slots               []SlotProposer
	ProducedSlotHeights map[uint64]struct{}
	BlocksInWindow      []TickBlockHeader // narrow shape — only Signers needed

	Reshares []ReshareWithCommittee
	Blames   []BlameWithCommittee
	Signs    []SignResultWithCommittee
}

// TickBlockHeader is the narrow projection of vsc_blocks.VscHeaderRecord the
// aggregator consumes — only Signers matter for attestation scoring. Wrapping
// keeps rewards/ free of an import cycle and lets tests construct minimal
// fixtures.
type TickBlockHeader struct {
	Signers []string
}

// AggregateTick computes per-witness reward-reduction records for one tick:
// per-signal raw bps → per-signal pre-clamp at PerTickCapBps → max-of across
// signals → final clamp at PerTickCapBps. Returns records sorted
// lexicographically by Witness.
func AggregateTick(in TickInputs) []WitnessRewardReductionRecord {
	if len(in.Committee) == 0 {
		return nil
	}

	prod := ScoreBlockProduction(in.Slots, in.ProducedSlotHeights, in.Committee)
	att := scoreAttestationFromHeaders(in.BlocksInWindow, in.Committee)
	tssA := ScoreTssReshareExclusion(in.Reshares)
	tssB := ScoreTssBlame(in.Blames)
	tssC := ScoreTssSignNonParticipation(in.Signs)

	out := make([]WitnessRewardReductionRecord, 0, len(in.Committee))

	// Iterate committee in sorted order so output is deterministic.
	committee := append([]string(nil), in.Committee...)
	sort.Strings(committee)

	for _, w := range committee {
		ev := WitnessLivenessEvidence{
			BlockProductionBps:         clampPerTick(prod[w]),
			BlockAttestationBps:        clampPerTick(att[w]),
			TssReshareExclusionBps:     clampPerTick(tssA[w]),
			TssBlameBps:                clampPerTick(tssB[w]),
			TssSignNonParticipationBps: clampPerTick(tssC[w]),
		}
		bps := maxOfFive(
			ev.BlockProductionBps,
			ev.BlockAttestationBps,
			ev.TssReshareExclusionBps,
			ev.TssBlameBps,
			ev.TssSignNonParticipationBps,
		)
		bps = clampPerTick(bps)

		out = append(out, WitnessRewardReductionRecord{
			Witness:  w,
			Bps:      bps,
			Evidence: ev,
		})
	}

	return out
}

// scoreAttestationFromHeaders adapts the public ScoreBlockAttestation
// (which takes vsc_blocks.VscHeaderRecord) to the narrow TickBlockHeader the
// aggregator carries. Inlined as a thin loop — avoids the rewards/ package
// pulling vsc_blocks just to project Signers.
func scoreAttestationFromHeaders(blocks []TickBlockHeader, committee []string) map[string]int {
	if len(blocks) == 0 {
		return nil
	}
	cm := committeeFromMembers(committee)
	if len(cm) == 0 {
		return nil
	}
	out := make(map[string]int)
	for _, b := range blocks {
		signers := make(map[string]struct{}, len(b.Signers))
		for _, s := range b.Signers {
			signers[s] = struct{}{}
		}
		for member := range cm {
			if _, signed := signers[member]; signed {
				continue
			}
			out[member] += BlockAttestationMissBps
		}
	}
	return out
}

// SortedReductionAccounts returns the keys of an accumulated reduction map
// in lexicographic order — the canonical iteration order for building a
// SettlementPayload's reward-reductions list.
func SortedReductionAccounts(totals map[string]int) []string {
	if len(totals) == 0 {
		return nil
	}
	out := make([]string, 0, len(totals))
	for k := range totals {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func clampPerTick(v int) int {
	if v < 0 {
		return 0
	}
	if v > PerTickCapBps {
		return PerTickCapBps
	}
	return v
}

func maxOfFive(a, b, c, d, e int) int {
	m := a
	if b > m {
		m = b
	}
	if c > m {
		m = c
	}
	if d > m {
		m = d
	}
	if e > m {
		m = e
	}
	return m
}
