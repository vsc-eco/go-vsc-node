package poaseats_test

import (
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/db/vsc/poaseats"
)

// BR-2 in the build map: a "hive:" prefix mismatch does not error, it silently
// matches nothing — and in the election path "matches nothing" means an empty
// committee. Every existing consumer in the codebase (bond_gate,
// election-proposer, bond_lock) defends the same way; this pins that the
// registry's own helper does too.
func TestNormalizeAccountHandlesEveryPrefixForm(t *testing.T) {
	cases := map[string]string{
		"alice":       "alice",
		"hive:alice":  "alice",
		" hive:alice": "alice",
		"hive:alice ": "alice",
		"":            "",
		"hive:":       "",
		// Only the leading "hive:" is stripped — an account that merely contains
		// the substring must survive intact.
		"hivemind": "hivemind",
		// Case-folded, because the write path (governance.NormalizeAccount) folds
		// too and the two must agree. NOTE the scope this implies: this helper is
		// for HIVE ACCOUNT NAMES, which Hive constrains to lowercase at L1. It
		// must never be pointed at a case-sensitive identifier such as a DID —
		// folding one would corrupt it. The registry only ever holds Hive
		// accounts (from witness.Account and RequiredAuths), so that holds today.
		"Alice":        "alice",
		"did:key:z6Mk": "did:key:z6mk",
	}
	for in, want := range cases {
		if got := poaseats.NormalizeAccount(in); got != want {
			t.Fatalf("NormalizeAccount(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSeatedDistinguishesNeverSeatedFromExited(t *testing.T) {
	never := poaseats.Seat{Account: "alice"}
	if never.Seated() {
		t.Fatal("a seat that has never been elected reports as seated")
	}
	seated := poaseats.Seat{Account: "alice", LastSeatedHeight: 100}
	if !seated.Seated() {
		t.Fatal("an elected seat with no exit does not report as seated")
	}
	exited := poaseats.Seat{Account: "alice", LastSeatedHeight: 100, ExitHeight: 200}
	if exited.Seated() {
		t.Fatal("an exited seat still reports as seated — the exit-halt would never arm")
	}
}

// Height-addressing is what makes a reindex reproduce the same elections as a
// live node. A read at height H must not see a seat admitted after H.
func TestGetSeatsAtHeightIsHeightAddressed(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	db.Seed("alice", "ubo-a", 100)
	db.Seed("bob", "ubo-b", 200)
	db.Seed("carol", "ubo-c", 300)

	for _, tc := range []struct {
		height uint64
		want   int
	}{{99, 0}, {100, 1}, {199, 1}, {200, 2}, {300, 3}, {10_000, 3}} {
		seats, err := db.GetSeatsAtHeight(tc.height)
		if err != nil {
			t.Fatalf("read at %d: %v", tc.height, err)
		}
		if len(seats) != tc.want {
			t.Fatalf("at height %d got %d seats, want %d — a seat visible before its admission height rewrites the past on reindex",
				tc.height, len(seats), tc.want)
		}
	}
}

// Every node must build the identical ordered set from the identical rows; the
// election is CID-committed, so an unordered read is a latent determinism bug.
func TestGetSeatsAtHeightIsDeterministicallyOrdered(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	for _, a := range []string{"zed", "alice", "mallory", "bob"} {
		db.Seed(a, "ubo-"+a, 10)
	}
	seats, err := db.GetSeatsAtHeight(10)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"alice", "bob", "mallory", "zed"}
	for i, w := range want {
		if seats[i].Account != w {
			t.Fatalf("seat[%d] = %s, want %s (accounts must come back sorted)", i, seats[i].Account, w)
		}
	}
}

// One operator, one seat — the property that makes per-UBO capping structural.
// A duplicate seat would double an operator's votes in the admit-vote tally.
func TestAdmitSeatRefusesDuplicateAccountAndDuplicateUbo(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	db.Seed("alice", "ubo-a", 100)

	if err := db.AdmitSeat(poaseats.Seat{Account: "alice", UboId: "ubo-other", AdmittedHeight: 200}); err == nil {
		t.Fatal("admitted a second seat for the same account")
	}
	// Prefixed form must collide with the bare one, not slip past as a new row.
	if err := db.AdmitSeat(poaseats.Seat{Account: "hive:alice", UboId: "ubo-other2", AdmittedHeight: 200}); err == nil {
		t.Fatal("admitted a duplicate seat via the hive: prefix form")
	}
	if err := db.AdmitSeat(poaseats.Seat{Account: "mallory", UboId: "ubo-a", AdmittedHeight: 200}); err == nil {
		t.Fatal("admitted a second seat for the same beneficial owner — one-seat-per-UBO is not enforced")
	}
	// A genuinely distinct operator still gets in.
	if err := db.AdmitSeat(poaseats.Seat{Account: "bob", UboId: "ubo-b", AdmittedHeight: 200}); err != nil {
		t.Fatalf("refused a legitimate distinct operator: %v", err)
	}
}

func TestAdmitSeatRefusesHeightZero(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	if err := db.AdmitSeat(poaseats.Seat{Account: "alice", UboId: "u", AdmittedHeight: 0}); err == nil {
		t.Fatal("admitted at height 0 — the seat would match every height-addressed read, including heights before it existed")
	}
}

// ★ The one that matters most for the halt: once an exit is recorded, later
// elections that also exclude the account must NOT push the height forward. If
// they did, an operator who exits and stays out would have their 3-day clock
// reset every election interval and the halt would never expire — a temporary
// lock silently becomes a permanent seizure of their bond.
func TestSetExitIsIdempotentSoTheHaltClockCannotRestart(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	db.Seed("alice", "ubo-a", 100)
	if err := db.SetSeating("alice", 500); err != nil {
		t.Fatal(err)
	}
	if err := db.SetExit("alice", 600); err != nil {
		t.Fatal(err)
	}
	for _, h := range []uint64{640, 680, 720} {
		if err := db.SetExit("alice", h); err != nil {
			t.Fatal(err)
		}
	}
	seat, _, _ := db.GetSeat("alice")
	if seat.ExitHeight != 600 {
		t.Fatalf("ExitHeight = %d after repeated exits, want 600 — the halt clock restarted and the bond would never release",
			seat.ExitHeight)
	}
}

// An account that has never been seated has no exit to record; writing one
// would arm a halt against an operator who never held keys.
func TestSetExitIgnoresNeverSeatedSeat(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	db.Seed("alice", "ubo-a", 100)
	if err := db.SetExit("alice", 600); err != nil {
		t.Fatal(err)
	}
	seat, _, _ := db.GetSeat("alice")
	if seat.ExitHeight != 0 {
		t.Fatalf("ExitHeight = %d for a never-seated account, want 0", seat.ExitHeight)
	}
}

// Re-entry must RE-ARM the halt (clear the exit), never leave a stale exit that
// would let a returning operator's bond release while they hold keys again.
func TestSetSeatingClearsExitOnReEntry(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	db.Seed("alice", "ubo-a", 100)
	_ = db.SetSeating("alice", 500)
	_ = db.SetExit("alice", 600)
	_ = db.SetSeating("alice", 700)

	seat, _, _ := db.GetSeat("alice")
	if seat.ExitHeight != 0 {
		t.Fatalf("ExitHeight = %d after re-entry, want 0 — a returning operator's bond must be locked again, not released", seat.ExitHeight)
	}
	if !seat.Seated() {
		t.Fatal("re-entered seat does not report as seated")
	}
	// And a subsequent exit starts a FRESH clock from the new departure.
	_ = db.SetExit("alice", 900)
	seat, _, _ = db.GetSeat("alice")
	if seat.ExitHeight != 900 {
		t.Fatalf("ExitHeight = %d after re-exit, want 900", seat.ExitHeight)
	}
}

// Consensus callers must be able to tell a transient read failure from a
// deterministic absence. Treating a failure as "no seat" opens the gate.
func TestReadFailureIsAnErrorNotAnAbsence(t *testing.T) {
	db := test_utils.NewMockPoaSeatsDb()
	db.Seed("alice", "ubo-a", 100)
	db.FailReads = true

	if _, _, err := db.GetSeat("alice"); err == nil {
		t.Fatal("GetSeat swallowed a read failure — callers cannot fail-stop on what they cannot see")
	}
	if _, err := db.GetSeatsAtHeight(100); err == nil {
		t.Fatal("GetSeatsAtHeight swallowed a read failure")
	}
}
