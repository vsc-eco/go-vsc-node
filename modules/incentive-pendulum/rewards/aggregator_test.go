package rewards

import (
	"testing"
)

func TestAggregateTick_NoCommittee(t *testing.T) {
	if got := AggregateTick(TickInputs{}); got != nil {
		t.Fatalf("expected nil for empty committee, got %v", got)
	}
}

// TestAggregateTick_MaxOfSuppressesLowerSignals: a tick where a witness
// missed everything should record bps == max of the per-signal values, not
// the sum. With weights {prod=200, att=25*4=100, tssC=30}, max is 200.
func TestAggregateTick_MaxOfSuppressesLowerSignals(t *testing.T) {
	in := TickInputs{
		Committee: []string{"alice"},
		Slots: []SlotProposer{
			{SlotHeight: 100, Account: "alice"},
		},
		ProducedSlotHeights: map[uint64]struct{}{}, // alice missed her slot
		BlocksInWindow: []TickBlockHeader{
			{Signers: []string{}}, // alice not in signers
			{Signers: []string{}},
			{Signers: []string{}},
			{Signers: []string{}},
		},
	}
	got := AggregateTick(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	rec := got[0]
	if rec.Witness != "alice" {
		t.Fatalf("witness: got %s want alice", rec.Witness)
	}
	if rec.Evidence.BlockProductionBps != 200 {
		t.Errorf("prod bps: got %d want 200", rec.Evidence.BlockProductionBps)
	}
	if rec.Evidence.BlockAttestationBps != 100 {
		t.Errorf("att bps: got %d want 100 (4 misses × 25)", rec.Evidence.BlockAttestationBps)
	}
	// max-of: max(200, 100, 0, 0, 0) = 200
	if rec.Bps != 200 {
		t.Errorf("consolidated bps: got %d want 200 (max-of)", rec.Bps)
	}
}

// TestAggregateTick_PerSignalPreCap: a single-signal raw value above
// PerTickCapBps should be clamped before max-of.
func TestAggregateTick_PerSignalPreCap(t *testing.T) {
	// 50 missed attestations × 25 bps = 1250 — over the 1000 cap.
	in := TickInputs{
		Committee:      []string{"alice"},
		BlocksInWindow: make([]TickBlockHeader, 50),
	}
	got := AggregateTick(in)
	if got[0].Evidence.BlockAttestationBps != PerTickCapBps {
		t.Fatalf("expected per-signal pre-cap at %d, got %d", PerTickCapBps, got[0].Evidence.BlockAttestationBps)
	}
	if got[0].Bps != PerTickCapBps {
		t.Fatalf("consolidated should also be %d, got %d", PerTickCapBps, got[0].Bps)
	}
}

// TestAggregateTick_SortedByWitness: ordering must be lexicographic across
// all returned records — `ComputeReductionsForEpoch` relies on stable
// ordering when accumulating across ticks.
func TestAggregateTick_SortedByWitness(t *testing.T) {
	in := TickInputs{
		Committee: []string{"zach", "alice", "mary"},
	}
	got := AggregateTick(in)
	if len(got) != 3 {
		t.Fatalf("expected 3 records, got %d", len(got))
	}
	if got[0].Witness != "alice" || got[1].Witness != "mary" || got[2].Witness != "zach" {
		t.Fatalf("not sorted: %+v", got)
	}
}
