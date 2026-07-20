package state_engine

import (
	"errors"
	"sort"
	"testing"

	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"

	"github.com/chebyrash/promise"
)

// These drive applyPoaSeatMaintenance directly (whitebox, same shape as
// bond_lock_internal_test.go) because it is the consensus point that creates
// the "when did this seat leave the set" fact — a fact that exists nowhere else
// in the codebase and that the collateral exit-halt is counted from.
//
// The doubles are LOCAL rather than lib/test_utils ones: test_utils imports
// state-processing (contract_test_utils.go), so an in-package test cannot import
// it back without an import cycle. They mirror the real registry's SEMANTICS —
// the idempotent exit and the uniqueness rules — because those semantics are
// exactly what is under test; a permissive double would let a test pass against
// behaviour the real store refuses.

type fakePlugin struct{}

func (fakePlugin) Init() error                  { return nil }
func (fakePlugin) Start() *promise.Promise[any] { return nil }
func (fakePlugin) Stop() error                  { return nil }

type fakeSeats struct {
	fakePlugin
	seats     map[string]poaseats.Seat
	failReads bool
}

func newFakeSeats() *fakeSeats { return &fakeSeats{seats: map[string]poaseats.Seat{}} }

func (f *fakeSeats) GetSeatsAtHeight(height uint64) ([]poaseats.Seat, error) {
	if f.failReads {
		return nil, errors.New("read failure")
	}
	out := make([]poaseats.Seat, 0, len(f.seats))
	for _, s := range f.seats {
		if s.AdmittedHeight <= height {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Account < out[j].Account })
	return out, nil
}

func (f *fakeSeats) GetSeat(account string) (poaseats.Seat, bool, error) {
	if f.failReads {
		return poaseats.Seat{}, false, errors.New("read failure")
	}
	s, ok := f.seats[poaseats.NormalizeAccount(account)]
	return s, ok, nil
}

func (f *fakeSeats) GetSeatByUbo(uboId string) (poaseats.Seat, bool, error) {
	if uboId == "" {
		return poaseats.Seat{}, false, nil
	}
	for _, s := range f.seats {
		if s.UboId == uboId {
			return s, true, nil
		}
	}
	return poaseats.Seat{}, false, nil
}

func (f *fakeSeats) AdmitSeat(seat poaseats.Seat) error {
	seat.Account = poaseats.NormalizeAccount(seat.Account)
	if seat.Account == "" {
		return errors.New("empty account")
	}
	if seat.AdmittedHeight == 0 {
		return errors.New("height 0")
	}
	if _, dup := f.seats[seat.Account]; dup {
		return errors.New("already seated")
	}
	if seat.UboId != "" {
		if _, dup, _ := f.GetSeatByUbo(seat.UboId); dup {
			return errors.New("ubo already holds a seat")
		}
	}
	f.seats[seat.Account] = seat
	return nil
}

func (f *fakeSeats) SetSeating(account string, height uint64) error {
	acct := poaseats.NormalizeAccount(account)
	s, ok := f.seats[acct]
	if !ok {
		return nil
	}
	s.LastSeatedHeight, s.ExitHeight = height, 0
	f.seats[acct] = s
	return nil
}

func (f *fakeSeats) SetExit(account string, height uint64) error {
	acct := poaseats.NormalizeAccount(account)
	s, ok := f.seats[acct]
	if !ok || s.LastSeatedHeight == 0 || s.ExitHeight != 0 {
		return nil
	}
	s.ExitHeight = height
	f.seats[acct] = s
	return nil
}

func (f *fakeSeats) seed(account, ubo string, admitted, seated uint64) {
	f.seats[account] = poaseats.Seat{
		Account: account, UboId: ubo,
		AdmittedHeight: admitted, LastSeatedHeight: seated,
	}
}

// fakeElections exists only so ActiveConsensusVersion can resolve a version.
type fakeElections struct {
	fakePlugin
	version uint64
}

func (f *fakeElections) StoreElection(elections.ElectionResult) error { return nil }
func (f *fakeElections) GetElection(uint64) *elections.ElectionResult { return nil }
func (f *fakeElections) GetElectionStrict(uint64) (*elections.ElectionResult, error) {
	return nil, nil
}
func (f *fakeElections) GetPreviousElections(uint64, int) []elections.ElectionResult { return nil }
func (f *fakeElections) GetElectionByHeight(uint64) (elections.ElectionResult, error) {
	return elections.ElectionResult{
		ElectionDataInfo: elections.ElectionDataInfo{ProtocolVersion: f.version},
	}, nil
}

// poaEnv builds the minimum StateEngine the seat-maintenance path needs.
func poaEnv(t *testing.T, chainConsensus uint64) (*StateEngine, *fakeSeats) {
	t.Helper()
	seats := newFakeSeats()
	return &StateEngine{poaSeats: seats, electionDb: &fakeElections{version: chainConsensus}}, seats
}

func ratified(epoch uint64, accounts ...string) elections.ElectionResult {
	members := make([]elections.ElectionMember, 0, len(accounts))
	for _, a := range accounts {
		members = append(members, elections.ElectionMember{Account: a})
	}
	return elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
		ElectionDataInfo:   elections.ElectionDataInfo{Members: members},
	}
}

// Below 0.7.0 the whole batch must be byte-identical to today: no registry
// writes at all, so an operator running this binary before the floor rises
// produces the same state as one running the old binary.
func TestSeatMaintenanceInertBelowActivation(t *testing.T) {
	se, seats := poaEnv(t, 3) // current mainnet floor
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob"), 100)
	if len(seats.seats) != 0 {
		t.Fatalf("wrote %d seats below the activation line — the batch is not inert", len(seats.seats))
	}
}

// ★ BR-1, the highest-consequence property in this build. The seat gate is an
// allowlist over candidacy; activated against an EMPTY registry it deletes every
// candidate and halts elections. That is not hypothetical — the structurally
// identical H-6 key gate starved the mainnet committee at epoch 1699 and is
// still disabled. Bootstrap seeding is what makes activation safe.
func TestBootstrapSeedsRegistryFromIncumbentCommittee(t *testing.T) {
	se, seats := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), 100)

	if len(seats.seats) != 3 {
		t.Fatalf("seeded %d seats, want 3 — an empty registry at activation would empty the committee", len(seats.seats))
	}
	for _, acct := range []string{"alice", "bob", "carol"} {
		seat, ok, _ := seats.GetSeat(acct)
		if !ok {
			t.Fatalf("%s was in the ratified committee but was not seeded", acct)
		}
		if !seat.Bootstrap {
			t.Fatalf("%s not marked Bootstrap — voted and seeded seats must stay distinguishable", acct)
		}
		if seat.AdmittedHeight != 100 {
			t.Fatalf("%s admitted at %d, want the election height 100", acct, seat.AdmittedHeight)
		}
		// Seeded seats are seated immediately: they ARE the current committee,
		// so their collateral must be locked from the moment the batch activates
		// rather than after they first appear in a later election.
		if !seat.Seated() {
			t.Fatalf("%s seeded but not seated — its bond would be withdrawable while it holds keys", acct)
		}
	}
}

// Seeding must happen exactly once. A second bootstrap would either duplicate
// seats (doubling an operator's admit-vote weight) or, if it silently replaced
// them, wipe voted-in seats and their UBO bindings.
func TestBootstrapHappensOnlyOnce(t *testing.T) {
	se, seats := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob"), 100)
	se.applyPoaSeatMaintenance(ratified(11, "alice", "bob", "carol"), 200)

	if len(seats.seats) != 2 {
		t.Fatalf("registry has %d seats after a second election, want 2 — carol was never voted in and must not be seeded", len(seats.seats))
	}
	if _, ok, _ := seats.GetSeat("carol"); ok {
		t.Fatal("carol was seeded by a later election — bootstrap must be a one-time event, otherwise the allowlist is no allowlist at all")
	}
}

// The core B1 fact: leaving the ratified set records an exit height, which is
// what the 3-day collateral halt is counted from.
func TestExitHeightRecordedWhenSeatLeavesTheSet(t *testing.T) {
	se, seats := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob"), 100) // bootstrap
	se.applyPoaSeatMaintenance(ratified(11, "alice", "bob"), 200) // both still in
	se.applyPoaSeatMaintenance(ratified(12, "alice"), 300)        // bob drops out

	alice, _, _ := seats.GetSeat("alice")
	if alice.ExitHeight != 0 || alice.LastSeatedHeight != 300 {
		t.Fatalf("alice: exit=%d lastSeated=%d, want exit=0 lastSeated=300", alice.ExitHeight, alice.LastSeatedHeight)
	}
	bob, _, _ := seats.GetSeat("bob")
	if bob.ExitHeight != 300 {
		t.Fatalf("bob exit=%d, want 300 — without this the exit-halt has no clock to count from", bob.ExitHeight)
	}
	if bob.Seated() {
		t.Fatal("bob still reports as seated after leaving the set")
	}
}

// ★ If later elections could push the exit height forward, an operator who
// exits and stays out would have their 3-day clock reset every election
// interval — the halt would never expire and a temporary lock would become a
// permanent seizure of their bond.
func TestRepeatedAbsenceDoesNotRestartTheHaltClock(t *testing.T) {
	se, seats := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob"), 100)
	se.applyPoaSeatMaintenance(ratified(11, "alice"), 200) // bob exits at 200
	for _, h := range []uint64{300, 400, 500} {
		se.applyPoaSeatMaintenance(ratified(12, "alice"), h)
	}
	bob, _, _ := seats.GetSeat("bob")
	if bob.ExitHeight != 200 {
		t.Fatalf("bob exit=%d after staying out for 3 more elections, want 200 — the halt clock restarted and the bond would never release", bob.ExitHeight)
	}
}

// B2: a seat dropped for a liveness fault keeps its registry row, so it is
// re-electable with NO re-vote. Re-entry must re-arm the halt rather than
// release it — grace restores the seat, it never accelerates a withdrawal.
func TestReEntryNeedsNoReVoteAndReArmsTheHalt(t *testing.T) {
	se, seats := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob"), 100)
	se.applyPoaSeatMaintenance(ratified(11, "alice"), 200)        // bob drops (liveness)
	se.applyPoaSeatMaintenance(ratified(12, "alice", "bob"), 300) // bob returns

	bob, ok, _ := seats.GetSeat("bob")
	if !ok {
		t.Fatal("bob's seat vanished when he was dropped — re-entry would need a fresh 2/3 vote")
	}
	if bob.ExitHeight != 0 {
		t.Fatalf("bob exit=%d after re-entry, want 0 — a returning operator holds keys again, so the bond must be locked again", bob.ExitHeight)
	}
	if bob.LastSeatedHeight != 300 {
		t.Fatalf("bob lastSeated=%d, want 300", bob.LastSeatedHeight)
	}
}

// A prefix mismatch does not error, it matches nothing — and here "matches
// nothing" means every seat is recorded as having exited simultaneously, arming
// the collateral halt against the entire committee at once.
func TestSeatMaintenanceMatchesPrefixedMemberAccounts(t *testing.T) {
	se, seats := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 50, 0)
	seats.seed("bob", "ubo-b", 50, 0)
	_ = seats.SetSeating("alice", 60)
	_ = seats.SetSeating("bob", 60)

	// Historical election records carry the "hive:" prefix.
	se.applyPoaSeatMaintenance(ratified(11, "hive:alice", "hive:bob"), 200)

	for _, acct := range []string{"alice", "bob"} {
		seat, _, _ := seats.GetSeat(acct)
		if seat.ExitHeight != 0 {
			t.Fatalf("%s recorded as exited at %d despite being in the committee — the hive: prefix broke the match and armed the halt against the whole set",
				acct, seat.ExitHeight)
		}
		if seat.LastSeatedHeight != 200 {
			t.Fatalf("%s lastSeated=%d, want 200", acct, seat.LastSeatedHeight)
		}
	}
}

// A transient registry read failure must not be read as "no seats". Treated as
// an empty registry it would trigger a spurious bootstrap that duplicates the
// committee; treated as "nobody is in the set" it would arm the halt against
// every operator at once. Either way the correct move is to do nothing.
func TestReadFailureSkipsMaintenanceEntirely(t *testing.T) {
	se, seats := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 50, 0)
	_ = seats.SetSeating("alice", 60)
	seats.failReads = true

	se.applyPoaSeatMaintenance(ratified(11, "bob"), 200)

	seats.failReads = false
	alice, _, _ := seats.GetSeat("alice")
	if alice.ExitHeight != 0 {
		t.Fatalf("alice exited at %d off a failed read — a Mongo blip must never arm the collateral halt", alice.ExitHeight)
	}
	if len(seats.seats) != 1 {
		t.Fatalf("registry has %d seats after a failed read, want 1 — a failed read triggered a bootstrap", len(seats.seats))
	}
}

// An election that yields no usable member accounts must leave the registry
// empty, because an empty registry keeps the seat gate INERT. Seeding nothing
// is the safe outcome; the dangerous one is a registry that exists but is
// wrong.
func TestBootstrapRefusesToSeedFromAnEmptyCommittee(t *testing.T) {
	se, seats := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10), 100)
	if len(seats.seats) != 0 {
		t.Fatalf("seeded %d seats from an empty committee", len(seats.seats))
	}
	// Also for members whose accounts normalise to nothing.
	se.applyPoaSeatMaintenance(ratified(11, "", "hive:"), 200)
	if len(seats.seats) != 0 {
		t.Fatalf("seeded %d seats from unusable member accounts", len(seats.seats))
	}
}

// A nil registry (harnesses that do not wire POA) must be a no-op, not a panic.
func TestSeatMaintenanceNoOpsWithoutARegistry(t *testing.T) {
	se := &StateEngine{}
	se.applyPoaSeatMaintenance(ratified(10, "alice"), 100)
}

// Guards the normalisation contract the whole build depends on.
func TestSeatAccountNormalisationContract(t *testing.T) {
	if poaseats.NormalizeAccount("hive:alice") != poaseats.NormalizeAccount("alice") {
		t.Fatal("prefixed and bare forms of the same account do not normalise equal")
	}
}
