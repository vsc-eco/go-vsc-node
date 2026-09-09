package tss

import (
	"context"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	ecKeyGen "github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
)

// shortPreParamsConfig wraps a real SystemConfig, overriding ONLY TssParams so the
// pre-parameter budget is short enough to assert the TIMEOUT path directly.
// Embedding the interface gives every other method for free.
type shortPreParamsConfig struct {
	systemconfig.SystemConfig
	tp params.TssParams
}

func (c shortPreParamsConfig) TssParams() params.TssParams { return c.tp }

// VR2-18 regression guard.
//
// KeyGenDispatcher.Start used to do a bare `<-tssMgr.preParams`. GeneratePreParams
// is best-effort: it TryLocks and, on contention or a generation failure, returns
// WITHOUT writing the channel. So that receive could block forever — and it runs
// inside the RunActions dispatcher loop while tssMgr.lock is held, released only
// after the loop. Every later RunActions call then TryLocks, logs "skipped, lock
// held by previous batch", and returns. One keygen against an empty pool therefore
// froze the node's ENTIRE TSS participation (all chains) until restart.
//
// The property under test: an empty pool must produce a bounded CLEAN FAILURE,
// never an unbounded wait.
func TestAwaitPreParams_EmptyPoolCleanFailsInsteadOfBlockingForever(t *testing.T) {
	mgr := &TssManager{
		// sconf nil on purpose: preParamsTimeout falls back to its default, and
		// the point is that SOME bound always applies.
		preParams: make(chan ecKeyGen.LocalPreParams, 1), // deliberately empty
	}

	// Bound the test itself so a regression shows up as a failure, not a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := mgr.awaitPreParams(ctx, "vr2-18-test")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a clean failure on an empty preparams pool, got nil error")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("awaitPreParams blocked past the bound — VR2-18 has regressed: " +
			"this is the unbounded wait that freezes all TSS participation while holding tssMgr.lock")
	}
}

// Positive control: a warm pool must return immediately and unchanged, so the
// bound is a no-op on the healthy path.
func TestAwaitPreParams_WarmPoolReturnsImmediately(t *testing.T) {
	mgr := &TssManager{preParams: make(chan ecKeyGen.LocalPreParams, 1)}
	mgr.preParams <- ecKeyGen.LocalPreParams{}

	start := time.Now()
	if _, err := mgr.awaitPreParams(context.Background(), "vr2-18-warm"); err != nil {
		t.Fatalf("warm pool must succeed, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("warm-pool read took %s; the bound must be a no-op when a value is ready", elapsed)
	}
}

// A cancelled context must also clean-fail rather than wait out the full budget.
func TestAwaitPreParams_CancelledContextCleanFails(t *testing.T) {
	mgr := &TssManager{preParams: make(chan ecKeyGen.LocalPreParams, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := mgr.awaitPreParams(ctx, "vr2-18-cancel"); err == nil {
		t.Fatal("expected a clean failure when the context is already cancelled")
	}
}


// The bound must come from the TIMER, not only from a caller's context.
//
// Without this, TestAwaitPreParams_EmptyPoolCleanFails passes purely because the
// test's own ctx expires — so deleting the timer case would leave it green while
// the production path (whose ctx lives as long as the session) blocked forever
// again. This test gives the call a context that never fires, so ONLY the
// configured PreParamsTimeout can end the wait.
func TestAwaitPreParams_TimerBoundsTheWaitIndependentlyOfContext(t *testing.T) {
	base := systemconfig.MainnetConfig()
	tp := base.TssParams()
	tp.PreParamsTimeout = 150 * time.Millisecond

	mgr := &TssManager{
		sconf:     shortPreParamsConfig{SystemConfig: base, tp: tp},
		preParams: make(chan ecKeyGen.LocalPreParams, 1), // empty
	}

	if got := mgr.preParamsTimeout(); got != 150*time.Millisecond {
		t.Fatalf("preParamsTimeout() = %s, want the configured 150ms", got)
	}

	done := make(chan error, 1)
	go func() {
		// context.Background() never cancels: only the timer can end this.
		_, err := mgr.awaitPreParams(context.Background(), "vr2-18-timer")
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from the configured budget")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the timer did not bound the wait — with a non-cancelling context " +
			"this is the original VR2-18 unbounded receive")
	}
}

// The documented fallback must apply when the network leaves PreParamsTimeout
// unset — which today is EVERY network, so this is the value actually in force.
func TestPreParamsTimeout_FallsBackWhenUnset(t *testing.T) {
	if got := (&TssManager{}).preParamsTimeout(); got != time.Minute {
		t.Fatalf("nil sconf: preParamsTimeout() = %s, want the 1m fallback", got)
	}
	base := systemconfig.MainnetConfig()
	if base.TssParams().PreParamsTimeout != 0 {
		t.Skip("mainnet now sets PreParamsTimeout; update this guard to the new value")
	}
	if got := (&TssManager{sconf: base}).preParamsTimeout(); got != time.Minute {
		t.Fatalf("unset PreParamsTimeout: got %s, want the 1m fallback", got)
	}
}

// B1 mechanism guard.
//
// The reshare fix works by assigning pool-generated pre-parameters to
// save.LocalPreParams before NewLocalParty. tss-lib only takes the fast path if
// they satisfy ValidateWithProof (ecdsa/resharing/round_2_new_step_1.go); a
// zero-valued LocalPreParams falls through to GeneratePreParams SYNCHRONOUSLY,
// inside round 2, under the party mutex — the B1 wedge.
//
// This pins the two halves of that contract:
//   - a zero value must NOT validate (so the pre-fix state really did generate
//     synchronously, i.e. the bug was real), and
//   - the fast path is keyed on ValidateWithProof, so if a tss-lib upgrade ever
//     changes that condition, supplying params would silently become a no-op and
//     the wedge would return with no test going red.
func TestB1_ZeroPreParamsDoNotValidate_SoTheFixMustSupplyRealOnes(t *testing.T) {
	var zero ecKeyGen.LocalPreParams

	if zero.Validate() {
		t.Fatal("a zero-valued LocalPreParams unexpectedly passes Validate(); " +
			"the B1 premise (reshare fell through to synchronous safe-prime generation) no longer holds")
	}
	if zero.ValidateWithProof() {
		t.Fatal("a zero-valued LocalPreParams unexpectedly passes ValidateWithProof(); " +
			"tss-lib would have taken the fast path and B1 would not have been a wedge")
	}
}
