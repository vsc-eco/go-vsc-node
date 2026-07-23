package state_engine

import (
	"errors"
	"fmt"
	"sort"
	"testing"

	systemconfig "vsc-node/modules/common/system-config"
	"vsc-node/modules/db/vsc/elections"
	"vsc-node/modules/db/vsc/poaseats"
	"vsc-node/modules/db/vsc/witnesses"

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
	seats map[string]poaseats.Seat
	// failReads fails EVERY read (used where the caller must not block).
	failReads bool
	// failReadsFor fails the next N reads then recovers, so the fail-stop
	// (blockingRetry) paths can be exercised without looping forever.
	failReadsFor int
	readAttempts int
}

func newFakeSeats() *fakeSeats { return &fakeSeats{seats: map[string]poaseats.Seat{}} }

func (f *fakeSeats) GetSeatsAtHeight(height uint64) ([]poaseats.Seat, error) {
	f.readAttempts++
	if f.failReads {
		return nil, errors.New("read failure")
	}
	if f.failReadsFor > 0 {
		f.failReadsFor--
		return nil, errors.New("transient read failure")
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
		return fmt.Errorf("%w: %s", poaseats.ErrSeatExists, seat.Account)
	}
	if seat.UboId != "" {
		if _, dup, _ := f.GetSeatByUbo(seat.UboId); dup {
			return fmt.Errorf("%w", poaseats.ErrUboExists)
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
	if s.LastSeatedHeight > height {
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

// fakeWitnesses models the election proposer's candidate set for the exit-halt's
// electability check. By default every account holding a seat is an electable
// witness (an active operator); disable(acct) simulates the operator winding
// down (disabling its witness), which is the real action that lets the halt
// clock start.
type fakeWitnesses struct {
	fakePlugin
	seats    *fakeSeats
	disabled map[string]uint64 // acct -> the height it disabled its witness (0 = active)
}

func newFakeWitnesses(seats *fakeSeats) *fakeWitnesses {
	return &fakeWitnesses{seats: seats, disabled: map[string]uint64{}}
}

// disable records that acct turned its witness off at height dh (a dated
// announcement, which is what the exit-halt's recent-activity check reads).
func (f *fakeWitnesses) disable(acct string, dh uint64) {
	f.disabled[poaseats.NormalizeAccount(acct)] = dh
}

func (f *fakeWitnesses) disabledAt(acct string) (uint64, bool) {
	dh, ok := f.disabled[poaseats.NormalizeAccount(acct)]
	return dh, ok
}

func (f *fakeWitnesses) GetWitnessesAtBlockHeight(bh uint64, _ ...witnesses.SearchOption) ([]witnesses.Witness, error) {
	out := []witnesses.Witness{}
	for acct := range f.seats.seats {
		// Electable (enabled) unless disabled at or before bh.
		if dh, ok := f.disabledAt(acct); ok && dh <= bh {
			continue
		}
		out = append(out, witnesses.Witness{Account: acct, Enabled: true, Height: 1})
	}
	return out, nil
}
func (f *fakeWitnesses) GetLastestWitnesses(_ ...witnesses.SearchOption) ([]witnesses.Witness, error) {
	return f.GetWitnessesAtBlockHeight(0)
}
func (f *fakeWitnesses) GetWitnessesByPeerId(_ []string, _ ...witnesses.SearchOption) ([]witnesses.Witness, error) {
	return nil, nil
}
func (f *fakeWitnesses) GetWitnessAtHeight(account string, bh *uint64) (*witnesses.Witness, error) {
	acct := poaseats.NormalizeAccount(account)
	if _, held := f.seats.seats[acct]; !held {
		return nil, nil // never a witness
	}
	if dh, ok := f.disabledAt(acct); ok && (bh == nil || dh < *bh) {
		// Latest announcement is the disable, dated at dh.
		return &witnesses.Witness{Account: acct, Enabled: false, Height: dh}, nil
	}
	// Active: an enabled announcement dated long ago (height 1).
	return &witnesses.Witness{Account: acct, Enabled: true, Height: 1}, nil
}
func (f *fakeWitnesses) StoreNodeAnnouncement(string) error                    { return nil }
func (f *fakeWitnesses) SetWitnessUpdate(witnesses.SetWitnessUpdateType) error { return nil }

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
func poaEnv(t *testing.T, chainConsensus uint64) (*StateEngine, *fakeSeats, *fakeWitnesses) {
	t.Helper()
	seats := newFakeSeats()
	wits := newFakeWitnesses(seats)
	return &StateEngine{
		poaSeats:   seats,
		witnessDb:  wits,
		electionDb: &fakeElections{version: chainConsensus},
		sconf:      systemconfig.MocknetConfig(),
	}, seats, wits
}

// ratified builds an election that CARRIES ITS OWN consensus version, because
// that is what applyPoaSeatMaintenance reads. Defaulting it to the POA line
// matters: a fixture whose version was 0 would silently skip the whole path and
// every assertion below would pass vacuously.
func ratified(epoch uint64, accounts ...string) elections.ElectionResult {
	return ratifiedAtVersion(7, epoch, accounts...)
}

func ratifiedAtVersion(consensus, epoch uint64, accounts ...string) elections.ElectionResult {
	members := make([]elections.ElectionMember, 0, len(accounts))
	for _, a := range accounts {
		members = append(members, elections.ElectionMember{Account: a})
	}
	return elections.ElectionResult{
		ElectionCommonInfo: elections.ElectionCommonInfo{Epoch: epoch},
		ElectionDataInfo: elections.ElectionDataInfo{
			Members:         members,
			ProtocolVersion: consensus,
		},
	}
}

// Below 0.7.0 the whole batch must be byte-identical to today: no registry
// writes at all, so an operator running this binary before the floor rises
// produces the same state as one running the old binary.
func TestSeatMaintenanceInertBelowActivation(t *testing.T) {
	se, seats, _ := poaEnv(t, 3) // current mainnet floor
	se.applyPoaSeatMaintenance(ratifiedAtVersion(3, 10, "alice", "bob"), nil, 100)
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
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), nil, 100)

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
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), nil, 100)
	se.applyPoaSeatMaintenance(ratified(11, "alice", "bob", "carol", "dave"), nil, 200)

	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats after a second election, want 3 — dave was never voted in and must not be seeded", len(seats.seats))
	}
	if _, ok, _ := seats.GetSeat("dave"); ok {
		t.Fatal("dave was seeded by a later election — bootstrap must be a one-time event, otherwise the allowlist is no allowlist at all")
	}
}

// The core B1 fact: leaving the ratified set records an exit height, which is
// what the 3-day collateral halt is counted from.
func TestExitHeightRecordedWhenSeatLeavesTheSet(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), nil, 100) // bootstrap
	se.applyPoaSeatMaintenance(ratified(11, "alice", "bob", "carol"), nil, 200) // all still in
	se.applyPoaSeatMaintenance(ratified(12, "alice", "carol"), nil, 300)        // bob drops out

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
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), nil, 100)
	se.applyPoaSeatMaintenance(ratified(11, "alice", "carol"), nil, 200) // bob exits at 200
	for _, h := range []uint64{300, 400, 500} {
		se.applyPoaSeatMaintenance(ratified(12, "alice", "carol"), nil, h)
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
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), nil, 100)
	se.applyPoaSeatMaintenance(ratified(11, "alice", "carol"), nil, 200)        // bob drops (liveness)
	se.applyPoaSeatMaintenance(ratified(12, "alice", "bob", "carol"), nil, 300) // bob returns

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
	se, seats, _ := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 50, 0)
	seats.seed("bob", "ubo-b", 50, 0)
	_ = seats.SetSeating("alice", 60)
	_ = seats.SetSeating("bob", 60)

	// Historical election records carry the "hive:" prefix.
	se.applyPoaSeatMaintenance(ratified(11, "hive:alice", "hive:bob"), nil, 200)

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

// ★ A transient registry read must NOT be treated as "no seats". Read as an
// empty registry it triggers a spurious bootstrap that duplicates the committee;
// read as "nobody is in the set" it arms the collateral halt against every
// operator at once. Both are consensus-divergent. The path therefore RETRIES
// until the read succeeds rather than proceeding on a partial answer.
func TestTransientReadIsRetriedNotTreatedAsEmpty(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 60)
	seats.failReadsFor = 3 // fail three times, then recover

	se.applyPoaSeatMaintenance(ratified(11, "alice"), nil, 200)

	if seats.readAttempts < 4 {
		t.Fatalf("read attempted %d times, want >=4 — a transient failure was not retried", seats.readAttempts)
	}
	alice, _, _ := seats.GetSeat("alice")
	if alice.ExitHeight != 0 {
		t.Fatalf("alice recorded as exited (%d) — a transient read was treated as an empty member set", alice.ExitHeight)
	}
	if alice.LastSeatedHeight != 200 {
		t.Fatalf("alice lastSeated=%d, want 200 — maintenance did not complete after the read recovered", alice.LastSeatedHeight)
	}
	if len(seats.seats) != 1 {
		t.Fatalf("registry has %d seats — a failed read triggered a bootstrap", len(seats.seats))
	}
}

// An election that yields no usable member accounts must leave the registry
// empty, because an empty registry keeps the seat gate INERT. Seeding nothing
// is the safe outcome; the dangerous one is a registry that exists but is
// wrong.
func TestBootstrapRefusesToSeedFromAnEmptyCommittee(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10), nil, 100)
	if len(seats.seats) != 0 {
		t.Fatalf("seeded %d seats from an empty committee", len(seats.seats))
	}
	// Also for members whose accounts normalise to nothing.
	se.applyPoaSeatMaintenance(ratified(11, "", "hive:"), nil, 200)
	if len(seats.seats) != 0 {
		t.Fatalf("seeded %d seats from unusable member accounts", len(seats.seats))
	}
}

// A nil registry (harnesses that do not wire POA) must be a no-op, not a panic.
func TestSeatMaintenanceNoOpsWithoutARegistry(t *testing.T) {
	se := &StateEngine{}
	se.applyPoaSeatMaintenance(ratified(10, "alice"), nil, 100)
}

// Guards the normalisation contract the whole build depends on.
func TestSeatAccountNormalisationContract(t *testing.T) {
	if poaseats.NormalizeAccount("hive:alice") != poaseats.NormalizeAccount("alice") {
		t.Fatal("prefixed and bare forms of the same account do not normalise equal")
	}
}

// ───── B1: the collateral exit-halt ─────
//
// The halt is what turns the bond from a formality into a deterrent. Theft
// cannot be PREVENTED — an operator holding threshold shares signs off-protocol
// and Bitcoin confirms in ~10 minutes whatever Magi does — so it is deterred by
// collateral the thief cannot extract before the theft is detected. Every test
// below is about that window actually binding.

// mocknet pins PoaExitHaltBlocks = 120.
const testHalt = uint64(120)

func TestExitHaltInertBelowActivation(t *testing.T) {
	se, seats, _ := poaEnv(t, 3)
	seats.seed("alice", "ubo-a", 10, 100)
	if se.IsPoaExitHalted("alice", 200) {
		t.Fatal("exit-halt fired below consensus 0.7.0 — the batch is not inert")
	}
}

// An account with no seat is not a POA operator and has no collateral POA has
// any claim over. Halting it would freeze ordinary users' unstakes.
func TestExitHaltIgnoresAccountsWithoutASeat(t *testing.T) {
	se, _, _ := poaEnv(t, 7)
	if se.IsPoaExitHalted("randomuser", 200) {
		t.Fatal("exit-halt fired for an account with no seat — ordinary unstakes would freeze")
	}
}

// ★ RG-1 FIX: an admitted-but-never-seated seat IS halted (armed from
// admission), because under POA it is electable the moment it holds a seat.
// This is the seat state a ratification-gap attacker occupies when it unstakes
// in the window before its first seating; holding it closes the gap. (This test
// asserted the OPPOSITE before RG-1 was closed.)
func TestExitHaltHoldsAnAdmittedNeverSeatedSeat(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 0) // admitted, never seated — the RG-1 gap state
	if !se.IsPoaExitHalted("alice", 200) {
		t.Fatal("exit-halt did NOT fire for an admitted seat — the ratification gap (RG-1) is open: a member can unstake before its first seating and drain the slashable pool while keeping its seat")
	}
}

// ★ Held while STILL SEATED. A thief who steals and simply stays in the set,
// enjoying the BTC, must not be able to walk the collateral out meanwhile.
func TestExitHaltHoldsWhileStillSeated(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 100) // seated, no exit recorded
	for _, h := range []uint64{100, 500, 10_000} {
		if !se.IsPoaExitHalted("alice", h) {
			t.Fatalf("bond released at height %d while the seat still holds keys", h)
		}
	}
}

// ★ The window itself: held for exactly PoaExitHaltBlocks after the exit
// election, then released. Too short and a thief walks the collateral out
// before detection; never releasing is a seizure, not a halt.
func TestExitHaltRunsForTheConfiguredWindowThenReleases(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 100)
	if err := seats.SetExit("alice", 500); err != nil {
		t.Fatal(err)
	}
	// She wound down EARLY (disabled her witness at 300), so by the time the seat
	// clock (exit 500 + window) governs, her recent-witness-activity hold has long
	// since expired and the exit clock is what releases her.
	wits.disable("alice", 300)
	release := 500 + testHalt // seat-clock release (exit + window), the later of the two

	for _, h := range []uint64{500, 501, release - 1} {
		if !se.IsPoaExitHalted("alice", h) {
			t.Fatalf("bond released at height %d, before the halt expires at %d", h, release)
		}
	}
	for _, h := range []uint64{release, release + 1, release + 10_000} {
		if se.IsPoaExitHalted("alice", h) {
			t.Fatalf("bond still held at height %d, after the halt expired at %d — a halt that never lifts is a seizure",
				h, release)
		}
	}
	// In [disable+window, exit+window) she is held on the seat clock with a fixed
	// release height the refusal message can quote.
	got, armed := se.PoaExitHaltReleaseHeight("alice", 460)
	if !armed || got != release {
		t.Fatalf("release height = %d (armed=%v), want %d — the refusal message would quote the wrong height", got, armed, release)
	}
}

// Fail-closed. The cost of holding on a bad read is a delayed withdrawal; the
// cost of releasing is a thief's collateral leaving during the detection window.
func TestExitHaltHoldsOnReadFailure(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 100)
	_ = seats.SetExit("alice", 500)
	seats.failReads = true

	if !se.IsPoaExitHalted("alice", 10_000) {
		t.Fatal("bond released on an unreadable registry — a Mongo blip would let a thief's collateral out")
	}
}

// ★ Termination — but only once the operator can no longer be elected.
// Dropping from ONE election is not enough: while still an electable witness the
// bond stays held (that is the F1 re-election-gap protection — a still-electable
// operator can be re-seated at the next election, so its bond must remain
// slashable). Release requires (a) it stops being electable AND (b) the window
// after its exit elapses.
func TestExitHaltTerminatesOnlyWhenNoLongerElectable(t *testing.T) {
	se, _, wits := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), nil, 100)

	if !se.IsPoaExitHalted("bob", 150) {
		t.Fatal("seated operator's bond is not held")
	}
	// bob drops from the next election but is STILL an electable witness.
	se.applyPoaSeatMaintenance(ratified(11, "alice", "carol"), nil, 200)
	if !se.IsPoaExitHalted("bob", 200) {
		t.Fatal("bond released the instant bob left — the steal-and-run escape")
	}
	// ★ F1: even long AFTER the exit window, while bob remains electable the bond
	// is held — he could be re-seated at any election, so it must stay slashable.
	if !se.IsPoaExitHalted("bob", 200+testHalt+10_000) {
		t.Fatal("bond released while bob is still an electable witness — the re-election gap (F1) is open")
	}
	// bob genuinely winds down: disables his witness at 10400. Release is a window
	// after his LAST witness activity (RG-1c: a recently-disabled witness may
	// still have an in-flight election), i.e. 10400 + window.
	wits.disable("bob", 10_400)
	if !se.IsPoaExitHalted("bob", 10_400+testHalt-1) {
		t.Fatal("bond released before a full window after bob's last witness activity")
	}
	if se.IsPoaExitHalted("bob", 10_400+testHalt) {
		t.Fatal("bond never releases even after winding down + full window — a seizure, not a delay")
	}
}

// ★ Re-entry must not shorten the halt, and a still-electable operator is held
// throughout regardless of the clock.
func TestReEntryDoesNotShortenTheHalt(t *testing.T) {
	se, _, wits := poaEnv(t, 7)
	se.applyPoaSeatMaintenance(ratified(10, "alice", "bob", "carol"), nil, 100)
	se.applyPoaSeatMaintenance(ratified(11, "alice", "carol"), nil, 200)        // bob exits
	se.applyPoaSeatMaintenance(ratified(12, "alice", "bob", "carol"), nil, 250) // bob returns

	// Held while back in the set (electable), regardless of the old exit clock.
	if !se.IsPoaExitHalted("bob", 400) {
		t.Fatal("bond released while bob is back in the set — re-entering shortened the halt instead of re-arming it")
	}
	// Leaving again and winding down (disable at 500) starts a fresh window from
	// the last witness activity.
	se.applyPoaSeatMaintenance(ratified(13, "alice", "carol"), nil, 500)
	wits.disable("bob", 500)
	if !se.IsPoaExitHalted("bob", 500+testHalt-1) {
		t.Fatal("second window not enforced")
	}
	if se.IsPoaExitHalted("bob", 500+testHalt) {
		t.Fatal("second window never expires")
	}
}

// ★ THE F1 REGRESSION GUARD (re-election gap): a seat that was seated, exited,
// and whose window has fully ELAPSED — but which is STILL an electable witness —
// must remain held. This is the exact state a re-election attacker occupies in
// the generation→ratification window; gating on the lagging exit clock alone
// (the partial fix) let it through.
func TestExitHaltHoldsAnElectableWitnessAfterWindowElapsed(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 100) // seated
	_ = seats.SetExit("alice", 200)       // exited at 200; window would end at 320
	// alice is STILL an electable witness (default) — she can be re-seated.
	_ = wits
	if !se.IsPoaExitHalted("alice", 200+testHalt+5_000) {
		t.Fatal("RG-1 REGRESSION (F1): an exited-but-still-electable seat released after its window — a re-election attacker can unstake in the next generation→ratification gap and drain the slashable pool while keeping a fresh seat")
	}
}

// ★ THE RG-1c REGRESSION GUARD (disable-in-the-gap): a seat that was seated,
// exited, and whose seat-clock window has fully ELAPSED, but which DISABLED its
// witness only recently, must remain held — the recent disable means it may be a
// member of an in-flight election (decided at generation, not yet ratified). The
// electability-only fix (44db7185) released it; the recent-witness-activity guard
// closes it.
func TestRG1c_ExitedElapsedButRecentlyDisabledIsHeld(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 10, 100) // seated
	_ = seats.SetExit("alice", 200)       // exited at 200; seat clock ends at 320
	// The attacker was electable at the pending election's generation and disables
	// only now, at 10000 — long after the seat clock (320) would have released.
	wits.disable("alice", 10_000)
	// Just after disabling, within a window: HELD, despite the seat clock being
	// long expired.
	if !se.IsPoaExitHalted("alice", 10_050) {
		t.Fatal("RG-1c REGRESSION: an exited-elapsed seat that just disabled its witness is not held — it can unstake in the generation→ratification gap and drain the slashable pool while being seated from the frozen generation membership")
	}
	// A full window after the last witness activity, it finally releases.
	if se.IsPoaExitHalted("alice", 10_000+testHalt) {
		t.Fatal("bond never releases even a full window after the last witness activity")
	}
}

// The bond of a genuinely never-serving admitted operator MUST have a release
// path (the F2 freeze-forever fix): once it disables its witness (not electable)
// and a full window passes with no witness activity, its bond releases.
func TestNeverSeatedSeatReleasesAfterDisablingWitness(t *testing.T) {
	se, seats, wits := poaEnv(t, 7)
	seats.seed("alice", "ubo-a", 100, 0) // admitted at 100, never seated

	// Still electable → held (RG-1 first-election protection).
	if !se.IsPoaExitHalted("alice", 150) {
		t.Fatal("admitted electable seat not held — RG-1 first-election gap open")
	}
	// Winds down (never served): disables witness at 200. Releases a window after
	// its last witness activity (200 + window).
	wits.disable("alice", 200)
	if !se.IsPoaExitHalted("alice", 200+testHalt-1) {
		t.Fatal("released before a full window after the last witness activity")
	}
	if se.IsPoaExitHalted("alice", 200+testHalt) {
		t.Fatal("F2 REGRESSION: a never-served admitted seat that disabled its witness is STILL held — the bond is frozen forever with no release path")
	}
}

// A nil registry must be a no-op, not a panic or a blanket freeze.
func TestExitHaltNoOpsWithoutARegistry(t *testing.T) {
	se := &StateEngine{}
	if se.IsPoaExitHalted("alice", 100) {
		t.Fatal("exit-halt fired with no registry wired — every unstake on the network would freeze")
	}
}

// ★ THE REGRESSION FOR THE BATCH-WIDE DEAD PATH.
//
// applyPoaSeatMaintenance used to resolve its activation check via
// ActiveConsensusVersion(blockHeight). GetElectionByHeight filters
// block_height $lt height, and the election being processed is stored AT that
// height — so that call returned the PREVIOUS election, the same row the
// transition check tests. The gate demanded that row be at/above the POA line
// while the transition check demanded it be below: a contradiction no state
// could satisfy, so bootstrap never fired on any node at any epoch. The
// registry stayed empty forever, the seat gate stayed inert, and the batch was
// permanently dead while flat weight still applied.
//
// This test models the real relationship — a PREVIOUS election below the line
// and a CURRENT election at it — which is precisely the shape the old doubles
// could not express, since they returned one fixed version for every query.
func TestBootstrapFiresAtTheRealActivationTransition(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)

	prev := ratifiedAtVersion(3, 10, "alice", "bob", "carol") // pre-POA
	curr := ratifiedAtVersion(7, 11, "alice", "bob", "carol") // first POA election

	se.applyPoaSeatMaintenance(curr, &prev, 200)

	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats after the activation transition, want 3 — bootstrap did not fire, so the seat gate stays inert forever and the entire POA batch is dead code while flat weight still applies",
			len(seats.seats))
	}
	for _, acct := range []string{"alice", "bob", "carol"} {
		if seat, ok, _ := seats.GetSeat(acct); !ok || !seat.Bootstrap {
			t.Fatalf("%s was not seeded as a bootstrap seat", acct)
		}
	}
}

// And the other half of the same contradiction: once past the transition, an
// empty registry must NOT be re-seeded, because that is the signature of a node
// that lost its collection rather than one that has not seeded yet.
func TestBootstrapDoesNotReFireAfterTheTransition(t *testing.T) {
	se, seats, _ := poaEnv(t, 7)

	prev := ratifiedAtVersion(7, 20, "alice", "bob", "carol") // already POA
	curr := ratifiedAtVersion(7, 21, "alice", "bob", "carol")

	se.applyPoaSeatMaintenance(curr, &prev, 300)

	if len(seats.seats) != 0 {
		t.Fatalf("registry re-seeded %d seats past the transition — a node that merely lost its collection would silently rebuild a different registry and diverge from its peers",
			len(seats.seats))
	}
}
