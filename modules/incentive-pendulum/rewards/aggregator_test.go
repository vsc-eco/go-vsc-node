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

// TestAggregateTick_OracleDivergencePopulatesEvidence verifies the new
// liveness signal lands on the per-witness Evidence record and contributes
// to the consolidated max-of bps without disturbing other signals.
func TestAggregateTick_OracleDivergencePopulatesEvidence(t *testing.T) {
	in := TickInputs{
		Committee:                []string{"alice", "bob"},
		DivergingOracleWitnesses: []string{"alice"},
	}
	got := AggregateTick(in)
	if len(got) != 2 {
		t.Fatalf("expected 2 records, got %d", len(got))
	}
	var aliceRec, bobRec WitnessRewardReductionRecord
	for _, r := range got {
		switch r.Witness {
		case "alice":
			aliceRec = r
		case "bob":
			bobRec = r
		}
	}
	if aliceRec.Evidence.OracleQuoteDivergenceBps != OracleQuoteDivergenceBps {
		t.Errorf("alice oracle bps: got %d want %d",
			aliceRec.Evidence.OracleQuoteDivergenceBps, OracleQuoteDivergenceBps)
	}
	if aliceRec.Bps != OracleQuoteDivergenceBps {
		t.Errorf("alice consolidated bps: got %d want %d (max-of with single non-zero signal)",
			aliceRec.Bps, OracleQuoteDivergenceBps)
	}
	if bobRec.Evidence.OracleQuoteDivergenceBps != 0 {
		t.Errorf("bob: oracle bps should be 0, got %d", bobRec.Evidence.OracleQuoteDivergenceBps)
	}
	if bobRec.Bps != 0 {
		t.Errorf("bob: consolidated bps should be 0, got %d", bobRec.Bps)
	}
}

// TestAggregateTick_OracleDivergenceTakenByMaxOf: when oracle divergence is
// the largest signal, the consolidated bps must equal it; when a larger
// signal is also present, max-of selects that one.
func TestAggregateTick_OracleDivergenceTakenByMaxOf(t *testing.T) {
	in := TickInputs{
		Committee: []string{"alice"},
		Slots: []SlotProposer{
			{SlotHeight: 100, Account: "alice"},
		},
		ProducedSlotHeights:      map[uint64]struct{}{}, // alice missed her slot (200 bps)
		DivergingOracleWitnesses: []string{"alice"},     // oracle bps 150
	}
	got := AggregateTick(in)
	if len(got) != 1 {
		t.Fatalf("expected 1 record, got %d", len(got))
	}
	// max(prod=200, oracle=150) = 200
	if got[0].Bps != BlockProductionMissBps {
		t.Errorf("consolidated bps: got %d want %d (block production dominates oracle here)",
			got[0].Bps, BlockProductionMissBps)
	}
	// Both raw values still recorded for explorers/disputes.
	if got[0].Evidence.OracleQuoteDivergenceBps != OracleQuoteDivergenceBps {
		t.Errorf("oracle evidence not preserved: got %d", got[0].Evidence.OracleQuoteDivergenceBps)
	}
	if got[0].Evidence.BlockProductionBps != BlockProductionMissBps {
		t.Errorf("prod evidence not preserved: got %d", got[0].Evidence.BlockProductionBps)
	}
}

// TestAggregateTick_SortedByWitness: persistence depends on byte-stable
// bson; ordering must be lexicographic across all returned records.
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
