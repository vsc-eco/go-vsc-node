package tss

import (
	"sync"
	"testing"

	"github.com/chebyrash/promise"
)

// stubDispatcher is a minimal Dispatcher used to populate actionMap.
type stubDispatcher struct {
	sessionId string
	keyId     string
}

func (s *stubDispatcher) Start() error      { return nil }
func (s *stubDispatcher) SessionId() string { return s.sessionId }
func (s *stubDispatcher) KeyId() string     { return s.keyId }
func (s *stubDispatcher) HandleP2P(msg []byte, from string, isBrcst bool, cmt string, fromCmt string) {
}
func (s *stubDispatcher) Done() *promise.Promise[DispatcherResult] { return nil }
func (s *stubDispatcher) Cleanup()                                 {}

func mgrWithSessions(d ...*stubDispatcher) *TssManager {
	m := &TssManager{actionMap: make(map[string]Dispatcher), bufferLock: sync.RWMutex{}}
	for _, x := range d {
		m.actionMap[x.sessionId] = x
	}
	return m
}

// VR2-09 regression guard.
//
// BlockTick's keyLocks map is rebuilt every block and was populated ONLY from
// actions generated in that block, so a reshare started at block N was invisible
// at the sign tick at N+k. The sign was scheduled while the reshare was still
// running its rounds on the same nodes; both timed out. Because a Pending vault
// generation is reshared every epoch (VR2-10), that repeated until activation
// never landed — 6 of 15 devnet rotations stalled.
//
// inFlightSessions is what makes those cross-block sessions visible.
func TestInFlightSessions_ClassifiesRunningCeremoniesAndSigns(t *testing.T) {
	mgr := mgrWithSessions(
		&stubDispatcher{sessionId: sessionPrefixReshare + "820-1-btc-main", keyId: "btc-main"},
		&stubDispatcher{sessionId: sessionPrefixKeygen + "800-0-btc-gen2", keyId: "btc-gen2"},
		&stubDispatcher{sessionId: sessionPrefixSign + "830-0-eth-main", keyId: "eth-main"},
	)

	ceremony, signing := mgr.inFlightSessions()

	if !ceremony["btc-main"] {
		t.Fatal("an in-flight RESHARE must lock its key: this is the session a later sign tick used to ignore")
	}
	if !ceremony["btc-gen2"] {
		t.Fatal("an in-flight KEYGEN must lock its key")
	}
	if !signing["eth-main"] {
		t.Fatal("an in-flight SIGN must be reported so a reshare defers to it")
	}
	// Kinds must not bleed into each other.
	if ceremony["eth-main"] {
		t.Fatal("a sign must not be classified as a ceremony (it would wrongly block itself)")
	}
	if signing["btc-main"] {
		t.Fatal("a reshare must not be classified as a sign")
	}
	// An unrelated key must stay schedulable.
	if ceremony["unrelated-key"] || signing["unrelated-key"] {
		t.Fatal("an unrelated key must not be locked")
	}
}

// An empty actionMap must lock nothing, so the healthy path is unchanged.
func TestInFlightSessions_EmptyLocksNothing(t *testing.T) {
	ceremony, signing := mgrWithSessions().inFlightSessions()
	if len(ceremony) != 0 || len(signing) != 0 {
		t.Fatalf("expected no locks from an empty actionMap, got ceremony=%v signing=%v", ceremony, signing)
	}
}

// Defensive: a dispatcher with no key id, or a nil entry, must be skipped rather
// than locking the empty-string key (which would silently block every unkeyed
// action).
func TestInFlightSessions_SkipsNilAndKeylessEntries(t *testing.T) {
	mgr := mgrWithSessions(&stubDispatcher{sessionId: sessionPrefixReshare + "1-0-", keyId: ""})
	mgr.actionMap[sessionPrefixSign+"2-0-x"] = nil

	ceremony, signing := mgr.inFlightSessions()
	if ceremony[""] || signing[""] {
		t.Fatal("an empty key id must never be locked")
	}
	if len(ceremony) != 0 || len(signing) != 0 {
		t.Fatalf("expected nothing locked, got ceremony=%v signing=%v", ceremony, signing)
	}
}

// B9 (GV-H8 family) regression guard.
//
// Both reshare participant sets are filtered by the gossip readiness set this
// node happened to receive, and the NEW set's SIZE feeds the VSS polynomial
// degree. The session id carried no participant information, so two nodes with
// different views joined the SAME session and aborted mid-protocol.
//
// Binding a fingerprint of both sets into the session id turns that into a clean
// miss: divergent nodes form different ids and retry, rather than contributing to
// a session whose degree they disagree with.
func TestParticipantSetTag_DiffersWhenTheChosenSetDiffers(t *testing.T) {
	p := func(accounts ...string) []Participant {
		out := make([]Participant, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, Participant{Account: a})
		}
		return out
	}
	oldSet := p("alice", "bob", "carol")

	full := participantSetTag(oldSet, p("alice", "bob", "carol"))
	missingOne := participantSetTag(oldSet, p("alice", "bob"))
	if full == missingOne {
		t.Fatal("a different NEW participant set must produce a different tag — " +
			"otherwise nodes that disagree on the set (and therefore on the VSS degree) " +
			"still join the same session, which is the GV-H8 failure")
	}

	// The OLD set is equally gossip-filtered, so it must bind too.
	if participantSetTag(p("alice", "bob"), p("alice")) == participantSetTag(p("alice", "carol"), p("alice")) {
		t.Fatal("a different OLD participant set must produce a different tag")
	}
}

// The tag must depend on MEMBERSHIP, not on ordering, or honest nodes that agree
// on the set but iterate it differently would needlessly fail to meet.
func TestParticipantSetTag_IsOrderIndependent(t *testing.T) {
	p := func(accounts ...string) []Participant {
		out := make([]Participant, 0, len(accounts))
		for _, a := range accounts {
			out = append(out, Participant{Account: a})
		}
		return out
	}
	a := participantSetTag(p("carol", "alice", "bob"), p("bob", "alice"))
	b := participantSetTag(p("alice", "bob", "carol"), p("alice", "bob"))
	if a != b {
		t.Fatalf("tag must be order-independent: %s != %s", a, b)
	}
}

// VR2-09 starvation bound (found by adversarial review of the fix itself).
//
// Deferring a reshare while a sign is in flight is right — a sign is one
// round-trip and starting a reshare on top makes both time out — but it must be
// BOUNDED. ROTATE_INTERVAL (100) is an exact multiple of SIGN_INTERVAL (50), so
// every rotate check lands on a sign tick, and keyLocks only blocks a NEW sign
// while a CEREMONY is running, never while another sign is. A key under
// continuous signing load could therefore be deferred at every single rotate
// check and NEVER reshare — a liveness bug the original fix introduced.
func TestDeferReshare_IsBoundedSoAKeyCannotBeStarved(t *testing.T) {
	mgr := &TssManager{reshareDeferrals: make(map[string]int)}

	for i := 1; i <= MAX_RESHARE_DEFERRALS; i++ {
		if !mgr.deferReshare("btc-main") {
			t.Fatalf("deferral %d/%d should still defer (a sign really is in flight)", i, MAX_RESHARE_DEFERRALS)
		}
	}
	if mgr.deferReshare("btc-main") {
		t.Fatalf("after %d consecutive deferrals the reshare MUST proceed: "+
			"one collided ceremony that retries beats a key that never rotates its shares",
			MAX_RESHARE_DEFERRALS)
	}
	// The streak resets after it fires, so the next contention window starts fresh.
	if !mgr.deferReshare("btc-main") {
		t.Fatal("the deferral streak must reset once the reshare has been allowed through")
	}
}

// A reshare that actually proceeds must clear the streak, so unrelated later
// contention gets its own full allowance rather than inheriting a stale count.
func TestDeferReshare_ProceedingClearsTheStreak(t *testing.T) {
	mgr := &TssManager{reshareDeferrals: make(map[string]int)}
	mgr.deferReshare("btc-main")
	mgr.deferReshare("btc-main")
	mgr.clearReshareDeferrals("btc-main")

	for i := 1; i <= MAX_RESHARE_DEFERRALS; i++ {
		if !mgr.deferReshare("btc-main") {
			t.Fatalf("after a clear, deferral %d should still defer (streak was not reset)", i)
		}
	}
}

// Deferral counts must be per-key: one busy key must not consume another's allowance.
func TestDeferReshare_CountsArePerKey(t *testing.T) {
	mgr := &TssManager{reshareDeferrals: make(map[string]int)}
	for i := 0; i <= MAX_RESHARE_DEFERRALS; i++ {
		mgr.deferReshare("busy-key")
	}
	if !mgr.deferReshare("quiet-key") {
		t.Fatal("a different key must get its own deferral allowance")
	}
}
