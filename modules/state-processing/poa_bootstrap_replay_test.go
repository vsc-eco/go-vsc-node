package state_engine

import (
	"fmt"
	"testing"

	"vsc-node/modules/db/vsc/elections"
)

// belowPoa is a ratified predecessor still under the POA line — the fact that
// identifies the activation transition.
func belowPoa(epoch uint64) *elections.ElectionResult {
	e := ratifiedAtVersion(3, epoch)
	return &e
}

func atPoa(epoch uint64) *elections.ElectionResult {
	e := ratifiedAtVersion(7, epoch)
	return &e
}

// ★ THE REGRESSION. A crash mid-bootstrap used to be PERMANENT.
//
// Bootstrap was reachable only through `if len(seats) == 0`. One surviving row
// made that false forever, so the replay fell into the seating/exit maintenance
// loop, which iterates the rows that EXIST and therefore can never write the
// ones that do not. Because seats are append-only with no delete path and the
// transition fires exactly once, the node was left permanently short and
// permanently divergent from every peer, with no repair path in the codebase.
//
// The registry is not merklized, so nothing downstream detects the divergence —
// it surfaces as a different gated committee, a different election CID, a failed
// BLS aggregate and a stalled epoch.
func TestBootstrapRepairsAPartialRegistryOnReplay(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)

	// The post-crash state, modelled directly rather than by injecting a write
	// error: AdmitSeat failures are fail-stop (blockingRetry blocks the slot
	// until the DB recovers), so a node cannot proceed past a failed write. The
	// way a registry actually ends up short is that the PROCESS dies mid-burst,
	// leaving whatever rows already landed. That is exactly this: three of the
	// five members present, written by the first pass at this same height.
	for _, acct := range []string{"alice", "bob", "erin"} {
		seats.seed(acct, "", 100, 100)
		st := seats.seats[acct]
		st.Bootstrap = true
		seats.seats[acct] = st
	}

	committee := ratified(10, "alice", "bob", "carol", "dave", "erin")
	prev := belowPoa(9)

	// PREMISE CHECK. Without a genuinely partial registry there is nothing to
	// repair and every assertion below would pass vacuously.
	if got := len(seats.seats); got != 3 {
		t.Fatalf("premise not established: registry holds %d seats, want a partial 3", got)
	}
	if _, ok, _ := seats.GetSeat("carol"); ok {
		t.Fatal("premise not established: carol is present, so the registry is not partial")
	}

	// Replay the SAME block, as the streamer does after a restart: it checkpoints
	// only once s.process() returns, so the block that was interrupted runs again.
	se.applyPoaSeatMaintenance(committee, prev, 100)

	if got := len(seats.seats); got != 5 {
		t.Fatalf("registry holds %d seats after replay, want all 5. A partial bootstrap is still "+
			"permanent: this node computes a different gated committee from its peers, and because "+
			"seats are append-only with no delete path and the transition fires once, nothing in the "+
			"codebase can repair it.", got)
	}
	for _, acct := range []string{"alice", "bob", "carol", "dave", "erin"} {
		seat, ok, _ := seats.GetSeat(acct)
		if !ok {
			t.Fatalf("%s is in the ratified committee but missing from the registry after replay", acct)
		}
		if !seat.Bootstrap || seat.AdmittedHeight != 100 {
			t.Fatalf("%s repaired with wrong provenance: bootstrap=%v admitted=%d, want true/100 — "+
				"a repaired seat must be indistinguishable from one written on the first pass, or "+
				"nodes disagree about the registry's contents",
				acct, seat.Bootstrap, seat.AdmittedHeight)
		}
	}

	// Rows written on the first pass must be untouched: re-running bootstrap has
	// to be convergent, never destructive.
	alice, _, _ := seats.GetSeat("alice")
	if alice.AdmittedHeight != 100 || !alice.Seated() {
		t.Fatalf("alice was altered by the replay (admitted=%d seated=%v) — bootstrap must only ever ADD",
			alice.AdmittedHeight, alice.Seated())
	}
}

// ★ THE NEGATIVE CONTROL, and the reason the predicate is not simply
// "prevBelowPoa". Removing the row-count guard makes the transition test the
// ONLY thing standing between a ratified election and a re-seed. If that test
// also accepted "predecessor unknown", every epoch on a node that cannot read
// its previous election would silently admit the current committee — accounts
// that were never voted in — and the append-only allowlist would become a
// rubber stamp.
func TestNilPredecessorDoesNotReSeedAPopulatedRegistry(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)

	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), belowPoa(9), 100)
	if len(seats.seats) != 3 {
		t.Fatalf("premise not established: bootstrap seeded %d seats, want 3", len(seats.seats))
	}

	// Predecessor unreadable, registry already populated, and the committee now
	// contains an account that never held a seat.
	se.applyPoaSeatMaintenance(ratified(11, "alice", "bob", "carol", "mallory"), nil, 200)

	if _, ok, _ := seats.GetSeat("mallory"); ok {
		t.Fatal("mallory was admitted by a nil-predecessor election. An unreadable predecessor is " +
			"absence of evidence, not evidence of a transition; treating it as one lets any elected " +
			"account write itself a permanent seat with no admit-vote.")
	}
	if len(seats.seats) != 3 {
		t.Fatalf("registry grew to %d seats without a vote, want 3", len(seats.seats))
	}
}

// A predecessor already at or above the POA line is not a transition, so a
// populated registry must be maintained, never re-seeded.
func TestActivatedPredecessorDoesNotReSeed(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), belowPoa(9), 100)

	se.applyPoaSeatMaintenance(ratified(11, "alice", "bob", "carol", "dave"), atPoa(10), 200)

	if _, ok, _ := seats.GetSeat("dave"); ok {
		t.Fatal("dave was seeded after activation — bootstrap must fire only at the transition")
	}
	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats, want 3", len(seats.seats))
	}
}

// The duplicate-error bookkeeping. Before the fix a duplicate left the OUTER
// err non-nil, so every account on a healthy replay was counted as FAILED and
// the partial-bootstrap alarm fired on the happy path. That was cosmetic while
// bootstrap ran at most once; it is not cosmetic now that replay is the normal
// case, because an alarm that cries wolf every replay is one the operator stops
// reading — and it is the alarm for the unrepairable failure.
func TestReplayOfACompleteRegistryIsNotReportedAsFailure(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	committee := ratified(10, "alice", "bob", "carol")
	prev := belowPoa(9)

	se.applyPoaSeatMaintenance(committee, prev, 100)
	before := fmt.Sprint(seats.seats)

	se.applyPoaSeatMaintenance(committee, prev, 100)

	if got := len(seats.seats); got != 3 {
		t.Fatalf("replay changed the registry size to %d, want 3 — re-running bootstrap must be a no-op", got)
	}
	if after := fmt.Sprint(seats.seats); after != before {
		t.Fatalf("replay mutated existing seats.\nbefore: %s\nafter:  %s", before, after)
	}
}
