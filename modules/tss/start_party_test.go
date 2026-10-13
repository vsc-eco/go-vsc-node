package tss

import (
	"testing"

	btss "github.com/bnb-chain/tss-lib/v3/tss"
)

// fakeStartParty is a partyStarter whose Start returns a chosen *btss.Error.
type fakeStartParty struct{ ret *btss.Error }

func (f fakeStartParty) Start() *btss.Error { return f.ret }

// TestStartPartyNilError guards the NT-2 typed-nil trap. btss Party.Start()
// returns a concrete *btss.Error; a nil *btss.Error assigned straight into an
// error interface is NON-nil, which made every successful ceremony start look
// like a failure (the first NT-2 rework shipped exactly that and 08 caught it on
// the devnet). startParty checks the typed pointer first, so a clean start yields
// a true nil error. The direct-assignment form (var e error = p.Start()) fails the
// first assertion.
func TestStartPartyNilError(t *testing.T) {
	var nilErr *btss.Error // what a successful Start() returns
	if err := startParty(fakeStartParty{ret: nilErr}); err != nil {
		t.Fatalf("startParty(nil *btss.Error) = %v (type %T), want a true nil error", err, err)
	}

	if err := startParty(fakeStartParty{ret: new(btss.Error)}); err == nil {
		t.Fatal("startParty(non-nil *btss.Error) = nil, want the error propagated")
	}

	// Document the trap startParty guards: a typed-nil *btss.Error boxed directly
	// into an error interface is NOT nil. If this ever fails, Go semantics changed
	// and startParty's typed check would be redundant.
	var boxed error = nilErr
	if boxed == nil {
		t.Fatal("a nil *btss.Error boxed into error should be non-nil (the bug startParty guards)")
	}
}
