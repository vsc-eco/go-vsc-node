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
