package rewards

import "testing"

// stubEpochProvider returns canned TickInputs per tick height.
type stubEpochProvider struct {
	byTick map[uint64]TickInputs
}

func (s stubEpochProvider) TickInputsForRange(tickHeight uint64) (TickInputs, bool) {
	in, ok := s.byTick[tickHeight]
	return in, ok
}

// missTick: alice misses her block-production slot (200 bps) and 4 attestations
// (100 bps); bob is clean. Per tick the consolidated max-of for alice is 200.
func missTick() TickInputs {
	return TickInputs{
		Committee:           []string{"alice", "bob"},
		Slots:               []SlotProposer{{SlotHeight: 1, Account: "alice"}},
		ProducedSlotHeights: map[uint64]struct{}{}, // alice produced nothing
		BlocksInWindow: []TickBlockHeader{
			{Signers: []string{"bob"}},
			{Signers: []string{"bob"}},
			{Signers: []string{"bob"}},
			{Signers: []string{"bob"}},
		},
	}
}

// TestComputeReductionEvidenceForEpoch_AccumulatesAndAlignsWithConsolidated
// runs 3 ticks of identical evidence and checks that (a) per-signal bps and the
// raw consolidated total accumulate across ticks, (b) a clean witness is
// omitted, and (c) the raw total lines up with ComputeReductionsForEpoch's
// pre-forgiveness accumulation (raw - PerEpochForgivenessBps).
func TestComputeReductionEvidenceForEpoch_AccumulatesAndAlignsWithConsolidated(t *testing.T) {
	const tickInterval = 100
	// firstTick = 100; ticks at 100, 200, 300.
	provider := stubEpochProvider{byTick: map[uint64]TickInputs{
		100: missTick(),
		200: missTick(),
		300: missTick(),
	}}

	ev := ComputeReductionEvidenceForEpoch(provider, 0, 300, tickInterval)
	if _, ok := ev["bob"]; ok {
		t.Fatalf("clean witness bob should be omitted, got %+v", ev["bob"])
	}
	alice, ok := ev["alice"]
	if !ok {
		t.Fatalf("expected alice in evidence map")
	}
	// 3 ticks × {prod 200, att 100, per-tick max 200}.
	if alice.Signals.BlockProductionBps != 600 {
		t.Errorf("prod bps: got %d want 600", alice.Signals.BlockProductionBps)
	}
	if alice.Signals.BlockAttestationBps != 300 {
		t.Errorf("att bps: got %d want 300", alice.Signals.BlockAttestationBps)
	}
	if alice.RawConsolidatedBps != 600 {
		t.Errorf("raw consolidated bps: got %d want 600 (3 × max-of 200)", alice.RawConsolidatedBps)
	}

	// The consolidated path must agree: post-forgiveness = raw - forgiveness.
	reductions := ComputeReductionsForEpoch(provider, 0, 300, tickInterval)
	wantConsolidated := alice.RawConsolidatedBps - PerEpochForgivenessBps
	if reductions["alice"] != wantConsolidated {
		t.Errorf("consolidated reduction: got %d want %d (raw %d - forgiveness %d)",
			reductions["alice"], wantConsolidated, alice.RawConsolidatedBps, PerEpochForgivenessBps)
	}
}

// TestComputeReductionEvidenceForEpoch_Empty covers the guard rails: no
// provider, zero interval, and an empty window all yield a nil map.
func TestComputeReductionEvidenceForEpoch_Empty(t *testing.T) {
	if got := ComputeReductionEvidenceForEpoch(nil, 0, 300, 100); got != nil {
		t.Errorf("nil provider: want nil, got %v", got)
	}
	empty := stubEpochProvider{byTick: map[uint64]TickInputs{}}
	if got := ComputeReductionEvidenceForEpoch(empty, 0, 300, 0); got != nil {
		t.Errorf("zero interval: want nil, got %v", got)
	}
	if got := ComputeReductionEvidenceForEpoch(empty, 300, 300, 100); got != nil {
		t.Errorf("empty window: want nil, got %v", got)
	}
	// Provider that scores no ticks (all unscoreable) → nil.
	if got := ComputeReductionEvidenceForEpoch(empty, 0, 300, 100); got != nil {
		t.Errorf("no scoreable ticks: want nil, got %v", got)
	}
}
