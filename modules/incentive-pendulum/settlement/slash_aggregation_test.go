package settlement

import (
	"testing"

	pendulum_oracle "vsc-node/modules/db/vsc/pendulum_oracle"
)

func snap(blockHeight uint64, entries ...pendulum_oracle.WitnessSlashRecord) pendulum_oracle.SnapshotRecord {
	return pendulum_oracle.SnapshotRecord{
		TickBlockHeight: blockHeight,
		WitnessSlashBps: entries,
	}
}

func TestAccumulateSlashBps_EmptyInput(t *testing.T) {
	if got := AccumulateSlashBps(nil); got != nil {
		t.Fatalf("nil input should yield nil, got %v", got)
	}
	if got := AccumulateSlashBps([]pendulum_oracle.SnapshotRecord{}); got != nil {
		t.Fatalf("empty slice should yield nil, got %v", got)
	}
}

func TestAccumulateSlashBps_SingleSnapshot(t *testing.T) {
	got := AccumulateSlashBps([]pendulum_oracle.SnapshotRecord{
		snap(100,
			pendulum_oracle.WitnessSlashRecord{Witness: "alice", Bps: 25},
			pendulum_oracle.WitnessSlashRecord{Witness: "bob", Bps: 50},
		),
	})
	if got["alice"] != 25 || got["bob"] != 50 {
		t.Fatalf("unexpected totals: %+v", got)
	}
}

func TestAccumulateSlashBps_AccumulatesAcrossSnapshots(t *testing.T) {
	got := AccumulateSlashBps([]pendulum_oracle.SnapshotRecord{
		snap(100, pendulum_oracle.WitnessSlashRecord{Witness: "alice", Bps: 25}),
		snap(101, pendulum_oracle.WitnessSlashRecord{Witness: "alice", Bps: 30}),
		snap(102,
			pendulum_oracle.WitnessSlashRecord{Witness: "alice", Bps: 5},
			pendulum_oracle.WitnessSlashRecord{Witness: "bob", Bps: 100},
		),
	})
	if got["alice"] != 60 {
		t.Errorf("alice: got %d want 60", got["alice"])
	}
	if got["bob"] != 100 {
		t.Errorf("bob: got %d want 100", got["bob"])
	}
}

func TestAccumulateSlashBps_CapsAtTenThousand(t *testing.T) {
	// A malformed snapshot stream cannot make per-witness bps overflow
	// the bps domain; downstream slash application caps at the per-epoch
	// hard cap (1000 bps) regardless.
	snaps := make([]pendulum_oracle.SnapshotRecord, 0, 200)
	for i := 0; i < 200; i++ {
		snaps = append(snaps, snap(uint64(100+i),
			pendulum_oracle.WitnessSlashRecord{Witness: "alice", Bps: 100},
		))
	}
	got := AccumulateSlashBps(snaps)
	if got["alice"] != 10000 {
		t.Fatalf("expected cap at 10000, got %d", got["alice"])
	}
}

func TestAccumulateSlashBps_ZeroAndEmptyEntriesSkipped(t *testing.T) {
	got := AccumulateSlashBps([]pendulum_oracle.SnapshotRecord{
		snap(100,
			pendulum_oracle.WitnessSlashRecord{Witness: "", Bps: 25},
			pendulum_oracle.WitnessSlashRecord{Witness: "alice", Bps: 0},
			pendulum_oracle.WitnessSlashRecord{Witness: "alice", Bps: -5},
			pendulum_oracle.WitnessSlashRecord{Witness: "bob", Bps: 10},
		),
	})
	if _, ok := got["alice"]; ok {
		t.Errorf("alice should be skipped (zero/negative bps), got %d", got["alice"])
	}
	if got["bob"] != 10 {
		t.Errorf("bob: got %d want 10", got["bob"])
	}
}

func TestSortedSlashAccounts_Lexicographic(t *testing.T) {
	got := SortedSlashAccounts(map[string]int{"zeta": 1, "alpha": 1, "mike": 1})
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
