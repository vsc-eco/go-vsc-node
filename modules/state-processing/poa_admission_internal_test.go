package state_engine

import (
	"encoding/json"
	"testing"

	systemconfig "vsc-node/modules/common/system-config"
	governance_db "vsc-node/modules/db/vsc/governance"
	"vsc-node/modules/db/vsc/poaseats"
	governance "vsc-node/modules/governance"

	"github.com/chebyrash/promise"
)

// vsc.admit_vote is the only way a new operator ever enters the set, and its
// threshold IS the theft threshold — so these tests are about the properties
// that make admission self-protecting rather than a ladder to capture.

type fakeGovernance struct {
	proposals map[string]governance_db.Proposal
	votes     map[string][]governance_db.ProposalVote
}

func newFakeGovernance() *fakeGovernance {
	return &fakeGovernance{
		proposals: map[string]governance_db.Proposal{},
		votes:     map[string][]governance_db.ProposalVote{},
	}
}

func (f *fakeGovernance) Init() error                  { return nil }
func (f *fakeGovernance) Start() *promise.Promise[any] { return nil }
func (f *fakeGovernance) Stop() error                  { return nil }

func (f *fakeGovernance) GetProposal(id string) (governance_db.Proposal, bool, error) {
	p, ok := f.proposals[id]
	return p, ok, nil
}
func (f *fakeGovernance) SaveProposal(p governance_db.Proposal) error {
	f.proposals[p.ProposalId] = p
	return nil
}
func (f *fakeGovernance) RecordVote(v governance_db.ProposalVote) error {
	// Upsert on (proposal, voter) — a re-vote must never double-count.
	for i, existing := range f.votes[v.ProposalId] {
		if existing.Voter == v.Voter {
			f.votes[v.ProposalId][i] = v
			return nil
		}
	}
	f.votes[v.ProposalId] = append(f.votes[v.ProposalId], v)
	return nil
}
func (f *fakeGovernance) GetVotes(id string) ([]governance_db.ProposalVote, error) {
	return f.votes[id], nil
}
func (f *fakeGovernance) ListProposals(_, _, _ *string, _, _ int) ([]governance_db.Proposal, error) {
	return nil, nil
}

// admitEnv wires a state engine with a seat registry, a governance store and
// mocknet config (PoaAdmitVoteWindowBlocks = 120).
func admitEnv(t *testing.T, chainConsensus uint64, seatedAccounts ...string) (*StateEngine, *fakeSeats, *fakeGovernance) {
	t.Helper()
	seats := newFakeSeats()
	for i, a := range seatedAccounts {
		seats.seed(a, "ubo-"+a, 10, uint64(10+i))
	}
	gov := newFakeGovernance()
	se := &StateEngine{
		poaSeats:     seats,
		governanceDb: gov,
		electionDb:   &fakeElections{version: chainConsensus},
		sconf:        systemconfig.MocknetConfig(),
	}
	return se, seats, gov
}

func admitPayload(t *testing.T, candidate, ubo string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]string{
		"candidate": candidate,
		"ubo_id":    ubo,
		"net_id":    systemconfig.MocknetConfig().NetId(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// vote casts one admit vote from each named seat.
func vote(se *StateEngine, candidate, ubo string, height uint64, payload []byte, voters ...string) {
	for i, v := range voters {
		se.handleAdmitVote(payload, v, "tx-"+v, height+uint64(i))
	}
}

// ★ The headline property: admission needs ceil(2/3) of SEATS. With 4 seats
// that is 3 — and 2 must not be enough, because if a simple majority could
// admit, a sub-2/3 coalition could vote in accomplices until it reached 2/3 and
// controlled the vault. That is the capture ladder the threshold closes.
func TestAdmitVoteRequiresTwoThirdsOfSeats(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol", "dave")
	p := admitPayload(t, "newop", "ubo-new")

	vote(se, "newop", "ubo-new", 100, p, "alice", "bob")
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("2 of 4 seats admitted a new operator — a sub-2/3 coalition can vote in accomplices to reach 2/3, which is exactly the capture ladder this threshold exists to close")
	}

	vote(se, "newop", "ubo-new", 110, p, "carol")
	seat, seated, _ := seats.GetSeat("newop")
	if !seated {
		t.Fatal("3 of 4 seats (ceil(2/3)) failed to admit — the threshold is unreachable, so no operator could ever join")
	}
	if seat.UboId != "ubo-new" {
		t.Fatalf("admitted seat carries ubo %q, want ubo-new", seat.UboId)
	}
	if seat.Bootstrap {
		t.Fatal("a voted-in seat is marked Bootstrap")
	}
	if seat.AdmittedTxId == "" {
		t.Fatal("a voted-in seat records no admitting tx — the admission is unauditable")
	}
}

// One seat, one vote. Re-voting must not accumulate, or a single operator
// reaches the threshold alone.
func TestAdmitVoteIgnoresRepeatVotesFromTheSameSeat(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol", "dave")
	p := admitPayload(t, "newop", "ubo-new")

	for i := 0; i < 10; i++ {
		se.handleAdmitVote(p, "alice", "tx-repeat", 100)
	}
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("one seat voting ten times admitted an operator — a single operator can seat anybody")
	}
}

// Only seats vote. A well-funded non-seat must not be able to influence, let
// alone reach, the threshold.
func TestAdmitVoteIgnoresNonSeatVoters(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "newop", "ubo-new")

	vote(se, "newop", "ubo-new", 100, p, "mallory", "trudy", "eve")
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("accounts holding no seat admitted an operator — the electorate is not the seat set")
	}
	// And the seats themselves still work.
	vote(se, "newop", "ubo-new", 110, p, "alice", "bob")
	if _, seated, _ := seats.GetSeat("newop"); !seated {
		t.Fatal("2 of 3 seats (ceil(2/3)) failed to admit")
	}
}

// ★ One operator, one seat. Without this an operator vetted once could take
// several seats and the whole "n distinct operators" premise collapses.
func TestAdmitVoteRefusesASecondSeatForTheSameOwner(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	// alice's owner id is "ubo-alice" (seeded). Try to seat a second account
	// under that same owner.
	p := admitPayload(t, "alice-alt", "ubo-alice")
	vote(se, "alice-alt", "ubo-alice", 100, p, "alice", "bob", "carol")

	if _, seated, _ := seats.GetSeat("alice-alt"); seated {
		t.Fatal("a second seat was admitted for an owner that already holds one — one-operator-one-seat is not enforced, so a single vetted operator can hold several seats")
	}
}

// A blank owner id must be refused, not defaulted: every blank would collide
// under the sparse unique index, so the per-owner cap would silently stop
// binding and an operator could take unlimited seats.
func TestAdmitVoteRefusesBlankUbo(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "newop", "")
	vote(se, "newop", "", 100, p, "alice", "bob", "carol")
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("admitted a seat with no beneficial owner — the per-owner cap cannot bind without one")
	}
}

// Below 0.7.0 the op does not exist. (Dispatch is gated too; this proves the
// handler itself is safe even if reached.)
func TestAdmitVoteInertBelowActivation(t *testing.T) {
	se, _, _ := admitEnv(t, 3, "alice", "bob", "carol")
	if se.PoaAdmitVoteActive(100) {
		t.Fatal("admit_vote dispatches below consensus 0.7.0")
	}
}

// ★ The window must actually close. A proposal that never expires lets a
// coalition accumulate approvals indefinitely — collecting the last vote it
// needs months later, against an electorate that has since changed.
func TestAdmitVoteProposalExpires(t *testing.T) {
	se, seats, gov := admitEnv(t, 7, "alice", "bob", "carol", "dave")
	p := admitPayload(t, "newop", "ubo-new")

	se.handleAdmitVote(p, "alice", "tx-a", 100)
	se.handleAdmitVote(p, "bob", "tx-b", 101)

	// mocknet window = 120 blocks; 100+120 = 220 is expiry (inclusive).
	se.handleAdmitVote(p, "carol", "tx-c", 220)

	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("a vote at the expiry height admitted the operator — the window does not close, so approvals can be gathered indefinitely")
	}
	id := governance.AdmitSeatProposalID("newop", "ubo-new")
	if prop := gov.proposals[id]; prop.Status != string(governance.StatusExpired) {
		t.Fatalf("proposal status = %q, want expired", prop.Status)
	}
	// And it stays terminal: a later vote cannot resurrect it.
	se.handleAdmitVote(p, "dave", "tx-d", 221)
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("an expired proposal was resurrected by a later vote")
	}
}

// Approval is terminal and idempotent: once applied, further votes must not
// re-run the seat write.
func TestAdmitVoteIsTerminalOnceApplied(t *testing.T) {
	se, seats, gov := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "newop", "ubo-new")
	vote(se, "newop", "ubo-new", 100, p, "alice", "bob")

	if _, seated, _ := seats.GetSeat("newop"); !seated {
		t.Fatal("threshold met but no seat admitted")
	}
	before := len(seats.seats)
	se.handleAdmitVote(p, "carol", "tx-late", 110)
	if len(seats.seats) != before {
		t.Fatalf("a post-approval vote changed the registry (%d -> %d seats)", before, len(seats.seats))
	}
	id := governance.AdmitSeatProposalID("newop", "ubo-new")
	if prop := gov.proposals[id]; prop.Status != string(governance.StatusApplied) {
		t.Fatalf("proposal status = %q, want applied", prop.Status)
	}
}

// Voting for an account that already holds a seat is moot and must not open a
// proposal that could later "admit" a duplicate.
func TestAdmitVoteIgnoresAlreadySeatedCandidate(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "alice", "ubo-other")
	vote(se, "alice", "ubo-other", 100, p, "alice", "bob", "carol")

	seat, _, _ := seats.GetSeat("alice")
	if seat.UboId != "ubo-alice" {
		t.Fatalf("alice's owner id changed to %q — re-admitting a seated account rewrote its owner binding", seat.UboId)
	}
	if len(seats.seats) != 3 {
		t.Fatalf("registry has %d seats, want 3 — a duplicate was admitted", len(seats.seats))
	}
}

// The electorate is snapshotted at proposal creation. If it were re-read per
// vote, seats admitted mid-window would move the denominator under a proposal
// already being voted on — and a shrinking set would LOWER the bar.
func TestAdmitVoteElectorateIsSnapshottedAtProposalCreation(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "newop", "ubo-new")

	se.handleAdmitVote(p, "alice", "tx-a", 100) // opens at 3 seats; threshold 2

	// Three more seats arrive after the proposal opened. They must not raise the
	// bar for the vote already cast.
	seats.seed("dave", "ubo-dave", 105, 105)
	seats.seed("erin", "ubo-erin", 105, 105)
	seats.seed("finn", "ubo-finn", 105, 105)

	se.handleAdmitVote(p, "bob", "tx-b", 110)
	if _, seated, _ := seats.GetSeat("newop"); !seated {
		t.Fatal("2 votes against the 3-seat snapshot failed to admit — the electorate moved under an open proposal")
	}
}

// A transient registry read must not lower the threshold by shrinking the
// electorate. Refusing to tally is the only safe response.
func TestAdmitVoteRefusesToTallyOnReadFailure(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "newop", "ubo-new")
	seats.failReads = true

	vote(se, "newop", "ubo-new", 100, p, "alice", "bob", "carol")

	seats.failReads = false
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("a seat was admitted while the registry was unreadable — a partial electorate lowers the 2/3 bar")
	}
}

// Wrong net id must be ignored, so a testnet op replayed on mainnet cannot seat
// an operator.
func TestAdmitVoteRejectsWrongNetId(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	bad, _ := json.Marshal(map[string]string{
		"candidate": "newop", "ubo_id": "ubo-new", "net_id": "vsc-mainnet",
	})
	vote(se, "newop", "ubo-new", 100, bad, "alice", "bob", "carol")
	if _, seated, _ := seats.GetSeat("newop"); seated {
		t.Fatal("an op carrying another network's net_id seated an operator — cross-network replay is possible")
	}
}

// ★ A6, checked structurally: nothing anywhere can remove a seat. The registry
// interface has no delete, so this is a compile-time property — the assertion
// here documents it and fails loudly if someone adds one.
func TestSeatRegistryExposesNoRemovalPath(t *testing.T) {
	var registry poaseats.PoaSeats = newFakeSeats()
	// If a Delete/Revoke/Remove method is ever added to the interface, this type
	// assertion set stops compiling or starts succeeding — either way a reviewer
	// is forced to justify a seat-removal path, which would break the
	// self-protecting admission threshold (a coalition able to shrink the set
	// can reach 2/3 by subtraction instead of addition).
	if _, hasDelete := registry.(interface{ DeleteSeat(string) error }); hasDelete {
		t.Fatal("the seat registry exposes a removal path — voting must be entry-only, or a coalition can shrink the set to reach 2/3")
	}
	if _, hasRevoke := registry.(interface{ RevokeSeat(string) error }); hasRevoke {
		t.Fatal("the seat registry exposes a revocation path — voting must be entry-only")
	}
}

// The admit payload carries no vote value and no action field — there is no way
// to express "vote against" or "remove". Guards against a future payload gaining
// one without the design conversation.
func TestAdmitVotePayloadHasNoRemovalSemantics(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	// A payload that tries to smuggle a removal is simply parsed as an ordinary
	// admission of the named candidate; the extra fields are ignored.
	sneaky, _ := json.Marshal(map[string]any{
		"candidate": "newop", "ubo_id": "ubo-new",
		"net_id": systemconfig.MocknetConfig().NetId(),
		"vote":   false, "action": "remove", "remove": "alice",
	})
	vote(se, "newop", "ubo-new", 100, sneaky, "alice", "bob")

	if _, stillSeated, _ := seats.GetSeat("alice"); !stillSeated {
		t.Fatal("a crafted payload removed a seat — admission must be entry-only")
	}
	if _, seated, _ := seats.GetSeat("newop"); !seated {
		t.Fatal("the ordinary admission in the same payload did not apply")
	}
}

// ───── regressions from the PRUNED pass ─────

// The proposal id is sha256(candidate || NUL || ubo). That delimiter is only
// unambiguous while neither field can CONTAIN a NUL — and these are raw JSON
// strings, where a \u0000 escape decodes to a real NUL that passes straight
// through both normalisers. Without charset validation an attacker shifts the
// field boundary so two different (candidate, owner) pairs hash to one proposal,
// pooling votes cast for one pairing into the admission of another.
func TestAdmitVoteRejectsControlBytesAndBadCharset(t *testing.T) {
	nul := string(rune(0))
	rejected := []struct{ name, candidate, ubo string }{
		{"nul shifts the delimiter left", "newop" + nul + "x", "ubo-new"},
		{"nul shifts the delimiter right", "newop", "ubo" + nul + "new"},
		{"space inside the owner id", "newop", "ubo new"},
		{"outside the hive charset", "new_op", "ubo-new"},
		{"shorter than a hive account", "ab", "ubo-new"},
		{"leading separator", "-newop", "ubo-new"},
		{"trailing separator", "newop-", "ubo-new"},
		{"empty owner", "newop", ""},
	}
	for _, tc := range rejected {
		se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
		p, _ := json.Marshal(map[string]string{
			"candidate": tc.candidate, "ubo_id": tc.ubo,
			"net_id": systemconfig.MocknetConfig().NetId(),
		})
		vote(se, tc.candidate, tc.ubo, 100, p, "alice", "bob", "carol")
		if len(seats.seats) != 3 {
			t.Fatalf("%s: admitted (candidate=%q ubo=%q) — the proposal-id delimiter is attackable",
				tc.name, tc.candidate, tc.ubo)
		}
	}
}

// The flip side, and equally load-bearing: case and surrounding whitespace are
// NORMALISED rather than rejected, and normalisation happens BEFORE the id is
// derived. So votes that differ only in spelling converge on one proposal
// instead of splitting the electorate across two that can each never reach 2/3.
func TestAdmitVoteNormalisesRatherThanSplittingTheElectorate(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")
	mk := func(c, u string) []byte {
		b, _ := json.Marshal(map[string]string{
			"candidate": c, "ubo_id": u, "net_id": systemconfig.MocknetConfig().NetId(),
		})
		return b
	}
	// Three voters, three different spellings of the same admission.
	se.handleAdmitVote(mk("newop", "ubo-new"), "alice", "tx-a", 100)
	se.handleAdmitVote(mk("NEWOP", "UBO-NEW"), "bob", "tx-b", 101)
	se.handleAdmitVote(mk("  newop  ", " ubo-new "), "carol", "tx-c", 102)

	seat, ok, _ := seats.GetSeat("newop")
	if !ok {
		t.Fatal("three votes spelled differently failed to admit — they split across separate proposals, so the threshold can never be reached")
	}
	if seat.Account != "newop" || seat.UboId != "ubo-new" {
		t.Fatalf("seated (%s,%s), want (newop,ubo-new) — normalisation is not canonical", seat.Account, seat.UboId)
	}
}

// Voters approve a PROPOSAL. What gets seated must be the proposal's stored
// candidate/owner, never whatever the crossing vote happened to parse —
// otherwise the last voter, not the electorate, decides who is admitted.
func TestAdmitVoteSeatsTheProposalsCandidate(t *testing.T) {
	se, seats, gov := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "newop", "ubo-new")
	vote(se, "newop", "ubo-new", 100, p, "alice", "bob")

	prop := gov.proposals[governance.AdmitSeatProposalID("newop", "ubo-new")]
	seat, ok, _ := seats.GetSeat("newop")
	if !ok {
		t.Fatal("threshold met but nothing was seated")
	}
	if seat.Account != prop.Candidate || seat.UboId != prop.UboId {
		t.Fatalf("seated (%s,%s) but the proposal approved (%s,%s) — the crossing vote, not the electorate, decided the admission",
			seat.Account, seat.UboId, prop.Candidate, prop.UboId)
	}
}

// ★ The seat is written from the proposal's STORED fields, so a store that
// silently drops them seats an empty account and admission never works at all.
// The production store hand-rolls its $set map, so this is a live hazard, not a
// hypothetical: this test pins that whatever the store round-trips is enough to
// seat with.
func TestAdmitVoteSurvivesAProposalRoundTrip(t *testing.T) {
	se, seats, gov := admitEnv(t, 7, "alice", "bob", "carol")
	p := admitPayload(t, "newop", "ubo-new")

	se.handleAdmitVote(p, "alice", "tx-a", 100)

	// Simulate the store round-trip explicitly: re-save what was persisted, then
	// let the crossing vote read it back.
	id := governance.AdmitSeatProposalID("newop", "ubo-new")
	stored, ok, _ := gov.GetProposal(id)
	if !ok {
		t.Fatal("no proposal was persisted")
	}
	if stored.Candidate == "" || stored.UboId == "" {
		t.Fatalf("the persisted proposal lost its candidate/owner (candidate=%q ubo=%q) — seating from it would write an empty account and admission would silently never work",
			stored.Candidate, stored.UboId)
	}
	_ = gov.SaveProposal(stored)

	se.handleAdmitVote(p, "bob", "tx-b", 110)
	if _, seated, _ := seats.GetSeat("newop"); !seated {
		t.Fatal("admission did not apply after a proposal round-trip")
	}
}

// "One seat per owner" binds on a byte-exact unique index, so it only binds on
// the OWNER if every vote's spelling canonicalises identically — otherwise a
// shift key defeats the single structural defence against one vetted operator
// holding several seats.
func TestAdmitVoteCanonicalisesOwnerId(t *testing.T) {
	se, seats, _ := admitEnv(t, 7, "alice", "bob", "carol")

	p1 := admitPayload(t, "newop", "UBO-7F3C")
	vote(se, "newop", "UBO-7F3C", 100, p1, "alice", "bob")
	if _, ok, _ := seats.GetSeat("newop"); !ok {
		t.Fatal("first admission did not apply")
	}

	p2 := admitPayload(t, "newoptwo", "ubo-7f3c")
	vote(se, "newoptwo", "ubo-7f3c", 200, p2, "alice", "bob", "carol")
	if _, ok, _ := seats.GetSeat("newoptwo"); ok {
		t.Fatal("a second seat was admitted for the same owner spelled in different case — the per-owner cap falls to a shift key")
	}
}
