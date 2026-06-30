package state_engine

import "testing"

// TestGenerateScheduleEmptyWitnessListNoPanic verifies the GV4-3 scheduler
// backstop: GenerateSchedule must not panic (it previously did
// witnessList[slot % uint64(len(witnessList))], an integer divide-by-zero when
// the list is empty) but instead return an empty schedule. This is the
// last-resort guard behind the TxElectionResult.ExecuteTx MinMembers reject; if
// a degenerate (0-member) election ever reaches scheduling, the node degrades to
// "no producer this round" rather than panic-crashing the whole network.
//
// The full multi-node behaviour (empty election accepted with the guard off ->
// every node crashes; rejected with the guard on -> chain stays alive) is proven
// on devnet by tests/devnet TestGV43SubMinElectionHalt / ...Guarded.
func TestGenerateScheduleEmptyWitnessListNoPanic(t *testing.T) {
	var seed [32]byte
	cases := map[string][]Witness{
		"empty": {},
		"nil":   nil,
	}
	for name, wl := range cases {
		t.Run(name, func(t *testing.T) {
			// Must not panic.
			sched := GenerateSchedule(100, wl, seed)
			if len(sched) != 0 {
				t.Fatalf("expected empty schedule for %s witness list, got %d slots", name, len(sched))
			}
		})
	}
}

// TestGenerateScheduleNonEmptyStillSchedules is a sanity check that the backstop
// did not change normal scheduling: a valid (>= MinMembers) witness set still
// yields a populated schedule.
func TestGenerateScheduleNonEmptyStillSchedules(t *testing.T) {
	var seed [32]byte
	wl := []Witness{{Account: "a"}, {Account: "b"}, {Account: "c"}}
	sched := GenerateSchedule(100, wl, seed)
	if len(sched) == 0 {
		t.Fatalf("expected a non-empty schedule for a %d-witness list", len(wl))
	}
	for i, slot := range sched {
		if slot.Account == "" {
			t.Fatalf("schedule slot %d has empty account", i)
		}
	}
}
