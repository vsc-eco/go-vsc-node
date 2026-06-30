package state_engine_test

// F13 — empty/sub-MinMembers election → slot % len(members) divide-by-zero halt.
// FIX: GenerateSchedule guards an empty witness list and returns an empty
// schedule (= no producer this round) instead of panicking on every node.
// Red→green: before the guard this panicked with "integer divide by zero".

import (
	"testing"

	stateEngine "vsc-node/modules/state-processing"
)

func TestF13_EmptyWitnessListDoesNotPanic(t *testing.T) {
	var seed [32]byte // deterministic zero seed is fine for this guard test

	var panicVal interface{}
	var sched []stateEngine.WitnessSlot
	func() {
		defer func() { panicVal = recover() }()
		sched = stateEngine.GenerateSchedule(100, []stateEngine.Witness{}, seed)
	}()

	if panicVal != nil {
		t.Fatalf("F13 REGRESSION: GenerateSchedule panicked on empty witness list (%v) — div-by-zero halt path open", panicVal)
	}
	if len(sched) != 0 {
		t.Fatalf("F13: expected empty schedule for empty witness list, got %d slots", len(sched))
	}
	t.Log("F13 FIXED: empty witness list yields empty schedule, no divide-by-zero panic")
}
