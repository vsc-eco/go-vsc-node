package islock_attestation

import (
	"sync"
	"sync/atomic"
)

// singleflightMap coordinates duplicate calls. When N goroutines call
// do(key, fn) concurrently with the same key, only ONE runs fn(); the
// others wait for the result and return the same response. After fn()
// returns the entry is removed from the map (no in-memory persistence
// — the signRequestCache layer above handles cross-request memoisation).
//
// Audit R19-OPS-handle-request-not-single-flight-concurrent-
// rebroadcast-double-signs: the previous cache-then-Sign-then-put
// sequence admitted two concurrent rebroadcasts of the same cacheKey
// to both invoke Sign() (both miss the cache; both call Sign; the
// second put() noops as dup). Single-flight closes the window.
//
// Audit R20-CORR-singleflight-panic-leaks-followers-forever (HIGH):
// the leader's `resp := fn()` is wrapped in defer/recover so a
// panicking Sign (BLS library edge case, OOM, nil deref from a
// malformed memory entry, etc.) doesn't strand the cacheKey forever
// + leak every blocked follower. The deferred cleanup ALWAYS runs:
//  1. delete the call entry from the map (under s.mu)
//  2. close call.done so followers unblock
//
// On panic the leader leaves call.resp == nil; followers return
// (nil, false) — the same shape as a Sign error — and the outer
// pubsub.handleMessage's utils.RecoverGoroutine catches the panic
// for the leader goroutine.
//
// Audit R20-CONS-go-vsc-singleflight-embedded-by-value-go-vet-fails
// (HIGH): the singleflightMap field on Service was previously a
// value-embedded sync.Mutex carrier. go vet's copylocks pass would
// flag any value-receiver method (or function arg) that touches
// Service.signSF. The field is now an explicit pointer on the
// Service struct so the lock-holding subobject is never copied.
//
// This is a hand-rolled subset of golang.org/x/sync/singleflight to
// avoid promoting that package from indirect to direct dep just for
// the one call site. Behaviour parity: same key → one in-flight call
// + N-1 followers wait on a chan; followers see exactly the result
// the leader produced (no race-with-cache-eviction).
type singleflightMap struct {
	mu    sync.Mutex
	calls map[string]*singleflightCall
}

type singleflightCall struct {
	done chan struct{}
	resp *IsLockAttestationResponse
	// waiters counts follower goroutines that have entered the "wait
	// on <-done" branch. Exposed for tests (audit R21-CORR-go-vsc-
	// singleflight-test-eventually-cond-does-not-actually-prove-
	// follower-registered) so the singleflight regression test can
	// gate its barrier release on N-1 followers actually parked,
	// not just on "leader registered + cacheKey exists in map".
	// Incremented under s.mu in do(); read under s.mu via
	// waitersForKey() — both with s.mu held → no atomic strictly
	// required, but atomic.Int64 keeps the field race-detector-clean
	// even if a future refactor lifts the read out of s.mu.
	waiters atomic.Int64
}

func newSingleflightMap() *singleflightMap {
	return &singleflightMap{
		calls: make(map[string]*singleflightCall),
	}
}

// do runs fn() iff no other goroutine is currently running it for the
// same key. Returns (resp, true) on a successful flight (leader or
// follower); returns (nil, false) if fn returns nil OR if fn panics.
// Callers treat ok=false the same as a cache-miss-then-Sign-error.
//
// Panic-safety: the leader-side cleanup (delete call entry + close
// done channel) runs in a defer that fires even if fn() panics.
// After cleanup the panic re-raises so the outer goroutine's
// recover (utils.RecoverGoroutine on the pubsub dispatcher) logs
// + swallows it. Followers wake with call.resp == nil and return
// (nil, false). Audit R20-CORR-singleflight-panic-leaks-followers-
// forever.
func (s *singleflightMap) do(key string, fn func() *IsLockAttestationResponse) (*IsLockAttestationResponse, bool) {
	s.mu.Lock()
	if call, ok := s.calls[key]; ok {
		// Follower — register on the waiters counter (under s.mu so
		// the count is monotonic relative to the leader's defer-time
		// observation) then wait for the leader.
		call.waiters.Add(1)
		s.mu.Unlock()
		<-call.done
		return call.resp, call.resp != nil
	}
	// Leader — register the call before unlocking so concurrent
	// followers see it.
	call := &singleflightCall{done: make(chan struct{})}
	s.calls[key] = call
	s.mu.Unlock()

	// Cleanup ALWAYS runs. Ordering matters:
	//   1. Take s.mu + delete the entry first so a new flight after
	//      this one can register cleanly.
	//   2. Close done LAST so followers wake AFTER the map is clean
	//      (avoids the rare window where a wakened follower's caller
	//      re-enters do() and races against this leader's delete).
	defer func() {
		s.mu.Lock()
		delete(s.calls, key)
		s.mu.Unlock()
		close(call.done)
	}()

	// Run fn outside the map mutex so followers can register while
	// the leader is signing. A panic here unwinds through the defer
	// above (cleanup runs) and propagates to the outer caller; the
	// pubsub dispatcher's RecoverGoroutine catches it.
	resp := fn()
	call.resp = resp
	return resp, resp != nil
}

// waitersForKey returns the count of currently-parked followers on
// key, or 0 if no flight is in progress. Test-only — used by the
// singleflight regression tests to confirm followers ARE parked
// before releasing the leader's barrier (audit R21-CORR-go-vsc-
// singleflight-test-eventually-cond-does-not-actually-prove-
// follower-registered).
func (s *singleflightMap) waitersForKey(key string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if call, ok := s.calls[key]; ok {
		return call.waiters.Load()
	}
	return 0
}
