package devnet

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// withLead runs f with the package test clock's lead raised by extra, then puts
// the lead back (tests only: the real lead never goes down).
func withLead(t *testing.T) func(extra time.Duration) {
	t.Helper()
	saved := testClockLead.Load()
	t.Cleanup(func() { testClockLead.Store(saved) })
	return func(extra time.Duration) { raiseLead(&testClockLead, testClockLead.Load()+int64(extra)) }
}

func TestRaiseLeadOnlyMovesForward(t *testing.T) {
	var lead atomic.Int64
	raiseLead(&lead, 5)
	raiseLead(&lead, 3)
	if got := lead.Load(); got != 5 {
		t.Fatalf("lead = %d after raising to 5 then 3, want 5", got)
	}
	raiseLead(&lead, 9)
	if got := lead.Load(); got != 9 {
		t.Fatalf("lead = %d after raising to 9, want 9", got)
	}
}

// With no lead the test clock is the wall clock: a sleep is never shorter.
func TestDevnetSleepIsNeverShorterThanWallWithoutLead(t *testing.T) {
	withLead(t)
	start := time.Now() // wall: measures the sleep
	devnetSleep(120 * time.Millisecond)
	if el := time.Since(start); el < 120*time.Millisecond { // wall: measures the sleep
		t.Fatalf("devnetSleep(120ms) returned after %s of wall clock", el)
	}
}

// Chain time racing ahead ends a wait on the test clock early, as many blocks in.
func TestDevnetWaitsFollowTheLead(t *testing.T) {
	bump := withLead(t)
	slept := make(chan struct{})
	go func() {
		devnetSleep(time.Hour)
		close(slept)
	}()
	after := devnetAfter(time.Hour)
	ctx, cancel := devnetWithTimeout(context.Background(), time.Hour)
	defer cancel()

	time.Sleep(50 * time.Millisecond) // wall: let the waits start
	select {
	case <-slept:
		t.Fatal("devnetSleep(1h) returned with no lead")
	case <-after:
		t.Fatal("devnetAfter(1h) fired with no lead")
	case <-ctx.Done():
		t.Fatal("devnetWithTimeout(1h) expired with no lead")
	default:
	}

	bump(time.Hour)
	for name, ch := range map[string]<-chan struct{}{"devnetSleep": slept, "devnetWithTimeout": ctx.Done()} {
		select {
		case <-ch:
		case <-time.After(2 * time.Second): // wall: test bound
			t.Fatalf("%s(1h) still waiting after the clock moved 1h", name)
		}
	}
	select {
	case <-after:
	case <-time.After(2 * time.Second): // wall: test bound
		t.Fatal("devnetAfter(1h) did not fire after the clock moved 1h")
	}
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expired context Err = %v, want DeadlineExceeded", ctx.Err())
	}
	if dl, ok := ctx.Deadline(); !ok || devnetUntil(dl) > 0 {
		t.Fatalf("expired context Deadline = %v, %v; want one already passed on the test clock", dl, ok)
	}
}

func TestDevnetWithTimeoutCancellation(t *testing.T) {
	withLead(t)
	parent, cancelParent := context.WithCancel(context.Background())
	ctx, cancel := devnetWithTimeout(parent, time.Hour)
	defer cancel()
	cancelParent()
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second): // wall: test bound
		t.Fatal("parent cancel did not reach the test-clock context")
	}
	if !errors.Is(ctx.Err(), context.Canceled) {
		t.Fatalf("Err after parent cancel = %v, want Canceled", ctx.Err())
	}

	ctx2, cancel2 := devnetWithTimeout(context.Background(), time.Hour)
	cancel2()
	<-ctx2.Done()
	if !errors.Is(ctx2.Err(), context.Canceled) {
		t.Fatalf("Err after cancel = %v, want Canceled", ctx2.Err())
	}

	ctx3, cancel3 := devnetWithTimeout(context.Background(), 0)
	defer cancel3()
	select {
	case <-ctx3.Done():
	case <-time.After(2 * time.Second): // wall: test bound
		t.Fatal("zero timeout never expired")
	}
	if !errors.Is(ctx3.Err(), context.DeadlineExceeded) {
		t.Fatalf("zero timeout Err = %v, want DeadlineExceeded", ctx3.Err())
	}
}

// The pacemaker raises the lead to 3s of chain time per block it produced less
// the wall time that took.
func TestPacemakerRaisesTheTestClockLead(t *testing.T) {
	h := newPMHarness(t, 3)
	var lead atomic.Int64
	h.leadSink = &lead
	h.start()
	h.waitActivated()
	start := time.Now()                // wall: measures the run
	time.Sleep(400 * time.Millisecond) // wall: let the pacemaker run
	h.halt()
	elapsed := time.Since(start) // wall: measures the run

	blocks := int64(len(h.hived.gens()))
	if blocks < 5 {
		t.Fatalf("want several fast blocks, got %d", blocks)
	}
	// Every block came after takeover, the last one at least ~350ms of wall clock
	// after it, and that wall time comes off the chain time produced.
	got := time.Duration(lead.Load())
	chain := time.Duration(blocks) * hiveBlockInterval
	max := chain - 200*time.Millisecond
	min := chain - elapsed - 500*time.Millisecond
	if got <= 0 || got > max || got < min {
		t.Fatalf("lead %s after %d blocks (%s of chain) in %s, want within [%s, %s]", got, blocks, chain, elapsed, min, max)
	}
	if pkg := testClockLead.Load(); pkg != 0 && pkg == lead.Load() {
		t.Fatalf("unit-test pacemaker wrote the package test clock")
	}
}

// A goroutine blocked in devnetSleep or devnetAfter counts as the test waiting,
// and its wait wakes the pacemaker; a finished or zero wait does not count.
func TestTestClockWaitsCountAsTheTestWaiting(t *testing.T) {
	select { // drain a stale wake from another test
	case <-testWaitStarted:
	default:
	}
	if testWaiting() {
		t.Fatal("testWaiting() with no wait in progress")
	}
	devnetSleep(0)
	if testWaiting() {
		t.Fatal("a zero sleep left the test waiting")
	}
	done := make(chan struct{})
	go func() {
		devnetSleep(150 * time.Millisecond)
		close(done)
	}()
	select {
	case <-testWaitStarted:
	case <-time.After(time.Second): // wall: bound on the test itself
		t.Fatal("devnetSleep did not wake the pacemaker")
	}
	if !testWaiting() {
		t.Fatal("testWaiting() false during devnetSleep")
	}
	<-done
	if testWaiting() {
		t.Fatal("testWaiting() still true after devnetSleep returned")
	}
	<-devnetAfter(30 * time.Millisecond)
	time.Sleep(10 * time.Millisecond) // wall: let the timer goroutine return
	if testWaiting() {
		t.Fatal("testWaiting() still true after devnetAfter fired")
	}
}

// The harness holds real cadence while the test runs its own code, and lets the
// pacemaker fast-forward only while the test waits on the test clock.
func TestHarnessHoldIsTestRunningUnlessTheTestWaits(t *testing.T) {
	d := &Devnet{}
	if got := d.harnessHold(); got != "test running" {
		t.Fatalf("harnessHold() = %q with the test running, want \"test running\"", got)
	}
	done := make(chan struct{})
	go func() {
		devnetSleep(100 * time.Millisecond)
		close(done)
	}()
	time.Sleep(30 * time.Millisecond) // wall: let the wait start
	if got := d.harnessHold(); got != "" {
		t.Fatalf("harnessHold() = %q while the test waits, want \"\"", got)
	}
	<-done
}

// A block moves the lead by the chain time it stands for less the wall time it
// took, never backwards.
func TestLeadGain(t *testing.T) {
	for _, c := range []struct{ gap, want time.Duration }{
		{100 * time.Millisecond, 2900 * time.Millisecond},
		{3 * time.Second, 0},
		{9 * time.Second, 0},
	} {
		if got := leadGain(c.gap); got != c.want {
			t.Errorf("leadGain(%s) = %s, want %s", c.gap, got, c.want)
		}
	}
}

// Right after a hold longer than a block, fast blocks still move the test clock a
// whole block each: the hold is not repaid out of them (FD-23).
func TestPacemakerLeadAfterALongHold(t *testing.T) {
	h := newPMHarness(t, 3)
	var lead atomic.Int64
	h.leadSink = &lead
	h.maxGap = 10 * time.Second // the hold produces no block
	h.start()
	h.waitActivated()
	time.Sleep(100 * time.Millisecond) // wall: some fast blocks first
	h.setHeld("test running")
	time.Sleep(4500 * time.Millisecond) // wall: a hold of one and a half blocks
	before, beforeLead := len(h.hived.gens()), lead.Load()
	h.setHeld("")
	for deadline := time.Now().Add(2 * time.Second); len(h.hived.gens()) < before+3 && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) { // wall: test bound
	}
	afterLead := lead.Load()
	h.halt()
	if len(h.hived.gens()) < before+3 {
		t.Fatalf("want 3 fast blocks after the hold, got %d", len(h.hived.gens())-before)
	}
	// The first block after the hold took 4.5s and moves nothing; the next ones
	// move about 3s each. Repaying the hold would leave about 4.5s instead.
	if got := time.Duration(afterLead - beforeLead); got < 5500*time.Millisecond {
		t.Fatalf("lead moved %s over the 3 blocks after a 4.5s hold, want about 6s (two whole blocks)", got)
	}
}
