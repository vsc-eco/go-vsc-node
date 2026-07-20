package state_engine

import (
	"encoding/json"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"
	governance_db "vsc-node/modules/db/vsc/governance"
	"vsc-node/modules/db/vsc/poaseats"
	governance "vsc-node/modules/governance"

)

// FAILURE-STATE SUITE.
//
// PRUNED and the happy-path tests cover "does it work". This file covers "what
// does it do when something is already wrong" — nil dependencies, deterministic
// write refusals under a fail-stop retry loop, degenerate consensus inputs,
// cross-type store collisions, and the one KNOWN-open gap (the ratification
// window). Failure states are where the expensive bugs in this build lived:
// three of the four criticals only manifested under an adverse storage/ordering
// condition, not under normal flow.

// ---- A. blockingRetry must NOT loop forever on a DETERMINISTIC refusal ----
//
// bootstrap/admission writes run under blockingRetry, which retries until the
// write succeeds. A duplicate-seat error is DETERMINISTIC (every node sees it),
// so it must be classified and surfaced, never retried — retrying it wedges
// block processing forever. This is the failure the typed-error refactor closes.
//
// The test is wrapped in a timeout: if the classification regresses, bootstrap
// hangs, and we want that to FAIL the test (and free the machine), not spin.

// alwaysDupSeats: every AdmitSeat returns the DETERMINISTIC duplicate sentinel.
type alwaysDupSeats struct{ *fakeSeats }

func (a alwaysDupSeats) AdmitSeat(seat poaseats.Seat) error {
	return poaseats.ErrSeatExists // deterministic — must be surfaced, not retried
}

func TestBootstrapDoesNotHangOnADeterministicDuplicate(t *testing.T) {
	base := newFakeSeats()
	se := &StateEngine{
		poaSeats:   alwaysDupSeats{base},
		electionDb: &fakeElections{version: 7},
		sconf:      systemconfig.MocknetConfig(),
	}

	prev := ratifiedAtVersion(3, 10, "alice", "bob", "carol")
	done := make(chan struct{})
	go func() {
		// bootstrap seeds 3 members; every AdmitSeat returns ErrSeatExists.
		se.applyPoaSeatMaintenance(ratifiedAtVersion(7, 11, "alice", "bob", "carol"), &prev, 200)
		close(done)
	}()

	select {
	case <-done:
		// Correct: the deterministic refusal was classified and surfaced, so
		// blockingRetry returned instead of spinning.
	case <-time.After(5 * time.Second):
		t.Fatal("bootstrap hung on a deterministic duplicate-seat error — blockingRetry is looping on a non-transient failure, which wedges block processing forever")
	}
}

// ---- B. exit-halt fails CLOSED, never panics, on a missing config ----

func TestExitHaltFailsClosedWithoutConfig(t *testing.T) {
	// poaSeats wired but sconf nil — a construction path that wires the store
	// without config must not crash a consensus tx and must not release a bond.
	seats := newFakeSeats()
	seats.seed("alice", "ubo-a", 10, 60)
	se := &StateEngine{poaSeats: seats, electionDb: &fakeElections{version: 7}} // sconf nil

	// Must not panic, and must HOLD (fail-closed) rather than release.
	if !se.IsPoaExitHalted("alice", 10_000) {
		t.Fatal("exit-halt released (or the version gate ran) with nil config — a gate that can't evaluate deterministically must hold, not release")
	}
}

// ---- C. the SHARED proposal store must isolate admit_seat from the others ----

func TestAdmitVoteIgnoresAForeignProposalType(t *testing.T) {
	se, seats, gov := admitEnv(t, 7, "alice", "bob", "carol")

	// Pre-seed the shared store with a proposal that has the SAME id an admit
	// vote would compute, but a DIFFERENT type (as if a reserve_payout somehow
	// collided). The admit handler must refuse to act on it.
	id := governance.AdmitSeatProposalID("newop", "ubo-new")
	_ = gov.SaveProposal(governance_db.Proposal{
		ProposalId: id,
		Type:       string(governance.ProposalReservePayout), // foreign type
		Status:     string(governance.StatusOpen),
	})

	p := admitPayload(t, "newop", "ubo-new")
	vote(se, "newop", "ubo-new", 100, p, "alice", "bob", "carol")

	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("admit_vote acted on a proposal of a foreign type in the shared store — a reserve_payout/slash_restore row could be turned into a seat")
	}
}

func TestAdmitAndReservePayoutIdsNeverCollide(t *testing.T) {
	// Type-prefixed ids: even identical content across types must not collide.
	admitID := governance.AdmitSeatProposalID("alice", "ubo-x")
	payoutID := governance.ReservePayoutProposalID("alice", 100, "ubo-x", "tx1")
	if admitID == payoutID {
		t.Fatalf("admit and reserve-payout proposal ids collide (%s) — a vote for one would tally on the other in the shared store", admitID)
	}
	if admitID[:10] != "admit_seat" {
		t.Fatalf("admit id not type-prefixed: %s", admitID)
	}
}

// ---- D. RG-1 CHARACTERIZATION — the KNOWN-OPEN ratification gap ----
//
// This test PINS the current (buggy) behaviour so a fix is detectable: a seat
// that is about to be elected but whose SetSeating has not yet run
// (LastSeatedHeight == 0) is NOT halted, which is the window a validator uses to
// move its bond out of the slashable pool. When the gap is closed, this test
// should be inverted. It is a characterization test, deliberately asserting the
// wrong-but-current behaviour, and it is labelled so nobody mistakes it for a
// guarantee.
func TestRG1_NeverSeatedMemberIsNotHalted_KNOWN_GAP(t *testing.T) {
	se, seats := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 0) // admitted, LastSeatedHeight==0 (the gap state)

	if se.IsPoaExitHalted("alice", 50) {
		t.Fatal("behaviour changed: a never-seated seat is now halted. If the ratification gap (RG-1) was intentionally closed, INVERT this characterization test.")
	}
	// Documents the consequence: in this state an unstake would be permitted,
	// which is exactly the RG-1 finding. See phase1/FINDING-ratification-gap.md.
}

// ---- E. NIL-DEPENDENCY safety — no panic on any partial wiring ----

func TestAdmitVoteNoPanicOnNilDependencies(t *testing.T) {
	payload, _ := json.Marshal(map[string]string{
		"candidate": "x", "ubo_id": "u", "net_id": systemconfig.MocknetConfig().NetId(),
	})
	cases := []*StateEngine{
		{},                                                   // everything nil
		{poaSeats: newFakeSeats()},                           // no governance, no sconf
		{governanceDb: newFakeGovernance()},                  // no seats, no sconf
		{poaSeats: newFakeSeats(), governanceDb: newFakeGovernance()}, // no sconf
	}
	for i, se := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("case %d: handleAdmitVote panicked on a nil dependency: %v", i, r)
				}
			}()
			se.handleAdmitVote(payload, "alice", "tx", 100)
		}()
	}
}

// ---- F. DEGENERATE consensus inputs to seat maintenance ----

func TestSeatMaintenanceToleratesDegenerateElectionMembers(t *testing.T) {
	se, seats := poaEnv(t, 7)

	// Duplicate accounts, an empty account, and a prefix-only account mixed with
	// enough real members to clear the MinMembers floor (mocknet=3). Must not
	// panic, must not double-seat, must not seat an empty account. Sanitising
	// down to exactly 3 distinct valid members clears the floor; dropping one
	// more would (correctly) trip the floor guard and seed nothing — a separate
	// behaviour tested elsewhere.
	prev := ratifiedAtVersion(3, 10, "alice")
	curr := ratifiedAtVersion(7, 11, "alice", "alice", "", "hive:", "bob", "carol")

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("seat maintenance panicked on degenerate members: %v", r)
			}
		}()
		se.applyPoaSeatMaintenance(curr, &prev, 200)
	}()

	// alice/bob/carol seeded once each; empty + prefix-only dropped; no dup alice.
	if n := len(seats.seats); n != 3 {
		t.Fatalf("registry has %d seats, want 3 (alice+bob+carol, no empties, no duplicate) — degenerate members were not sanitised", n)
	}
	if _, ok, _ := seats.GetSeat("alice"); !ok {
		t.Fatal("alice not seeded")
	}
	if _, ok, _ := seats.GetSeat(""); ok {
		t.Fatal("an empty account was seeded")
	}
}

