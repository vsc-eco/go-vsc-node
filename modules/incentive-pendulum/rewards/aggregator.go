package rewards

import (
	"sort"

	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
)

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
// lexicographically by Witness so the caller can persist them with
// byte-stable bson encoding across nodes.
func AggregateTick(in TickInputs) []pendulum_oracle.WitnessRewardReductionRecord {
	if len(in.Committee) == 0 {
		return nil
	}

	prod := ScoreBlockProduction(in.Slots, in.ProducedSlotHeights, in.Committee)
	att := scoreAttestationFromHeaders(in.BlocksInWindow, in.Committee)
	tssA := ScoreTssReshareExclusion(in.Reshares)
	tssB := ScoreTssBlame(in.Blames)
	tssC := ScoreTssSignNonParticipation(in.Signs)

	out := make([]pendulum_oracle.WitnessRewardReductionRecord, 0, len(in.Committee))

	// Iterate committee in sorted order so persisted bson is deterministic.
	committee := append([]string(nil), in.Committee...)
	sort.Strings(committee)

	for _, w := range committee {
		ev := pendulum_oracle.WitnessLivenessEvidence{
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

		// Only emit records for witnesses with non-zero bps OR with any
		// evidence — a fully-clean witness gets a zero record so explorers
		// can show "you are at 0 bps" rather than "you have no record".
		// In practice, persisting every committee member's row keeps the
		// snapshot self-describing.
		out = append(out, pendulum_oracle.WitnessRewardReductionRecord{
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

// AggregateEpoch aggregates per-witness bps across an epoch's snapshot
// range. For each witness:
//   1. Sum the per-tick consolidated Bps across all snapshots
//   2. Subtract PerEpochForgivenessBps (clamped at 0)
//   3. Clamp the result to [0, PerEpochCapBps]
//
// Returns a map keyed by witness account; witnesses with zero effective bps
// are omitted so callers can iterate the result and only see those whose
// rewards are actually reduced.
func AggregateEpoch(snapshots []pendulum_oracle.SnapshotRecord) map[string]int {
	if len(snapshots) == 0 {
		return nil
	}
	totals := make(map[string]int)
	for _, snap := range snapshots {
		for _, entry := range snap.WitnessRewardReductions {
			if entry.Witness == "" || entry.Bps <= 0 {
				continue
			}
			totals[entry.Witness] += entry.Bps
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
