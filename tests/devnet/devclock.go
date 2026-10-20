package devnet

// The devnet test clock.
//
// Under FastBlocks the chain runs ahead of wall clock, so a wall-clock budget
// ("the vault drains within 3 minutes") would cover up to twelve times the blocks
// it covers on a live chain, and a liveness regression could pass. Devnet tests
// therefore measure deadlines, sleeps and phase budgets on this clock: wall clock
// plus a lead that each block faster than 3s raises by the difference (each Hive
// block is 3s of chain time, see leadGain), so every fast block moves the clock
// one whole block. The lead only grows, so the clock is monotonic and never
// slower than wall clock: a stalled chain still times out. At real cadence the
// lead stays zero and this is the wall clock. A budget of X covers at most about
// X/3s blocks, as on a live chain.
//
// Wall clock stays right for the timeout of a single RPC or docker call, a poll
// interval, a unique id, or measuring how long a run took. Such a line says so in
// a "// wall:" comment; TestDevnetCodeUsesTheTestClock rejects any other
// time.Now, time.Sleep, time.After, time.Since, time.Until or context.WithTimeout
// in the package.

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// testClockLead is how far the test clock is ahead of wall clock, in
// nanoseconds. Only the pacemaker moves it, and only forward.
var testClockLead atomic.Int64

// testClockPoll is how often a wait on the test clock re-checks it. The lead
// moves in block-sized steps, so polling is the only way to see it.
const testClockPoll = 25 * time.Millisecond

// testWaits counts goroutines blocked on the test clock (devnetSleep, and the
// timer behind devnetAfter). The pacemaker fast-forwards only while it is
// non-zero (Devnet.harnessHold): while the test runs its own code, reading the
// chain and deciding what to send, blocks come at real cadence, as they would
// on a live chain. Otherwise a test that read a height and acted a second later
// acted several fast blocks later than on a live chain (FD-21: in ShadowEmptyGen
// the reads between a wait and the createKey broadcast take about 0.5s, 4-5 fast
// blocks).
var testWaits atomic.Int64

// testWaitStarted wakes a pacemaker holding for "test running" as soon as a
// wait begins.
var testWaitStarted = make(chan struct{}, 1)

// testWaiting reports whether any goroutine is blocked on the test clock.
func testWaiting() bool { return testWaits.Load() > 0 }

// testWaiters maps each wait in progress to where it was called from, so the
// pacemaker's block log can say whose wait let a fast block through.
var testWaiters sync.Map // *int -> "file.go:line"

// activeTestWaiters lists the call sites of the waits in progress.
func activeTestWaiters() string {
	var sites []string
	testWaiters.Range(func(_, v any) bool {
		sites = append(sites, v.(string))
		return true
	})
	sort.Strings(sites)
	return strings.Join(sites, ",")
}

func callSite(skip int) string {
	_, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return "?"
	}
	return fmt.Sprintf("%s:%d", filepath.Base(file), line)
}

// raiseLead moves lead to at least ns.
func raiseLead(lead *atomic.Int64, ns int64) {
	for {
		cur := lead.Load()
		if ns <= cur || lead.CompareAndSwap(cur, ns) {
			return
		}
	}
}

func devnetNow() time.Time {
	return time.Now().Add(time.Duration(testClockLead.Load())) // wall: the clock itself
}

func devnetSince(t time.Time) time.Duration { return devnetNow().Sub(t) }

func devnetUntil(t time.Time) time.Duration { return t.Sub(devnetNow()) }

// devnetSleep blocks until d has passed on the test clock.
func devnetSleep(d time.Duration) { sleepFrom(d, callSite(1)) }

func sleepFrom(d time.Duration, site string) {
	deadline := devnetNow().Add(d)
	if devnetUntil(deadline) <= 0 {
		return
	}
	key := new(int)
	testWaiters.Store(key, site)
	defer testWaiters.Delete(key)
	testWaits.Add(1)
	defer testWaits.Add(-1)
	select {
	case testWaitStarted <- struct{}{}:
	default:
	}
	for {
		rem := devnetUntil(deadline)
		if rem <= 0 {
			return
		}
		time.Sleep(min(rem, testClockPoll)) // wall: re-check interval
	}
}

// devnetAfter is time.After on the test clock.
func devnetAfter(d time.Duration) <-chan time.Time {
	ch := make(chan time.Time, 1)
	site := callSite(1)
	go func() {
		sleepFrom(d, site)
		ch <- devnetNow()
	}()
	return ch
}

// testClockCtx is a context that expires on the test clock.
type testClockCtx struct {
	context.Context
	deadline time.Time // on the test clock
	expired  atomic.Bool
}

func (c *testClockCtx) Deadline() (time.Time, bool) {
	if parent, ok := c.Context.Deadline(); ok && parent.Before(c.deadline) {
		return parent, true
	}
	return c.deadline, true
}

func (c *testClockCtx) Err() error {
	if c.expired.Load() {
		return context.DeadlineExceeded
	}
	return c.Context.Err()
}

// devnetWithTimeout is context.WithTimeout on the test clock. Read its Deadline
// back with devnetUntil, not time.Until.
func devnetWithTimeout(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	inner, cancel := context.WithCancel(parent)
	c := &testClockCtx{Context: inner, deadline: devnetNow().Add(d)}
	go func() {
		for {
			rem := devnetUntil(c.deadline)
			if rem <= 0 {
				if inner.Err() == nil {
					c.expired.Store(true)
				}
				cancel()
				return
			}
			select {
			case <-inner.Done():
				return
			case <-time.After(min(rem, testClockPoll)): // wall: re-check interval
			}
		}
	}()
	return c, cancel
}
