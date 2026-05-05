package settlement

import (
	"testing"

	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
)

func snap(blockHeight uint64, entries ...pendulum_oracle.WitnessRewardReductionRecord) pendulum_oracle.SnapshotRecord {
	return pendulum_oracle.SnapshotRecord{
		TickBlockHeight:         blockHeight,
		WitnessRewardReductions: entries,
	}
}

func TestAccumulateRewardReductionBps_EmptyInput(t *testing.T) {
	if got := AccumulateRewardReductionBps(nil); got != nil {
		t.Fatalf("nil input should yield nil, got %v", got)
	}
	if got := AccumulateRewardReductionBps([]pendulum_oracle.SnapshotRecord{}); got != nil {
		t.Fatalf("empty slice should yield nil, got %v", got)
	}
}

func TestAccumulateRewardReductionBps_SingleSnapshotWithinBuffer(t *testing.T) {
	// Buffer = 250 bps; both witnesses fall under it → omitted.
	got := AccumulateRewardReductionBps([]pendulum_oracle.SnapshotRecord{
		snap(100,
			pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: 25},
			pendulum_oracle.WitnessRewardReductionRecord{Witness: "bob", Bps: 50},
		),
	})
	if len(got) != 0 {
		t.Fatalf("expected forgiveness buffer to absorb everything, got %+v", got)
	}
}

func TestAccumulateRewardReductionBps_ForgivenessBuffer(t *testing.T) {
	// alice accumulates 300 bps over 4 snapshots — buffer (250) leaves 50.
	got := AccumulateRewardReductionBps([]pendulum_oracle.SnapshotRecord{
		snap(100, pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: 75}),
		snap(101, pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: 75}),
		snap(102, pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: 75}),
		snap(103, pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: 75}),
	})
	if got["alice"] != 50 {
		t.Fatalf("alice: got %d want 50 (300 - 250 buffer)", got["alice"])
	}
}

func TestAccumulateRewardReductionBps_PerEpochCap(t *testing.T) {
	// A node fully offline for many ticks accumulates well past the cap; cap
	// at 10000 bps applies after subtracting the buffer.
	snaps := make([]pendulum_oracle.SnapshotRecord, 0, 200)
	for i := 0; i < 200; i++ {
		snaps = append(snaps, snap(uint64(100+i),
			pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: 1000},
		))
	}
	got := AccumulateRewardReductionBps(snaps)
	if got["alice"] != 10000 {
		t.Fatalf("expected cap at 10000, got %d", got["alice"])
	}
}

func TestAccumulateRewardReductionBps_ZeroAndEmptyEntriesSkipped(t *testing.T) {
	got := AccumulateRewardReductionBps([]pendulum_oracle.SnapshotRecord{
		snap(100,
			pendulum_oracle.WitnessRewardReductionRecord{Witness: "", Bps: 25},
			pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: 0},
			pendulum_oracle.WitnessRewardReductionRecord{Witness: "alice", Bps: -5},
			pendulum_oracle.WitnessRewardReductionRecord{Witness: "bob", Bps: 10},
		),
	})
	if _, ok := got["alice"]; ok {
		t.Errorf("alice should be skipped (zero/negative bps), got %d", got["alice"])
	}
	// bob: 10 bps < 250 buffer → also dropped.
	if _, ok := got["bob"]; ok {
		t.Errorf("bob should be absorbed by buffer, got %d", got["bob"])
	}
}

func TestSortedReductionAccounts_Lexicographic(t *testing.T) {
	got := SortedReductionAccounts(map[string]int{"zeta": 1, "alpha": 1, "mike": 1})
	want := []string{"alpha", "mike", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("len mismatch: got %d want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("index %d: got %s want %s", i, got[i], want[i])
		}
	}
}
