package islock_attestation

import "sync"

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
// This is a hand-rolled subset of golang.org/x/sync/singleflight to
// avoid promoting that package from indirect to direct dep just for
// the one call site. Behaviour parity: same key → one in-flight call
// + N-1 followers wait on a chan; followers see exactly the result
// the leader produced (no race-with-cache-eviction).
type singleflightMap struct {
	mu     sync.Mutex
	calls  map[string]*singleflightCall
	// initOnce defers map allocation so the zero value is usable
	// without a constructor.
	initOnce sync.Once
}

type singleflightCall struct {
	done chan struct{}
	resp *IsLockAttestationResponse
}

// do runs fn() iff no other goroutine is currently running it for the
// same key. Returns (resp, true) on a successful flight (leader or
// follower); returns (nil, false) ONLY if fn returns nil (i.e. the
// leader's Sign failed). Callers treat ok=false the same as a
// cache-miss-then-Sign-error.
func (s *singleflightMap) do(key string, fn func() *IsLockAttestationResponse) (*IsLockAttestationResponse, bool) {
	s.initOnce.Do(func() {
		s.calls = make(map[string]*singleflightCall)
	})
	s.mu.Lock()
	if call, ok := s.calls[key]; ok {
		// Follower — wait for the leader.
		s.mu.Unlock()
		<-call.done
		return call.resp, call.resp != nil
	}
	// Leader — register the call before unlocking so concurrent
	// followers see it.
	call := &singleflightCall{done: make(chan struct{})}
	s.calls[key] = call
	s.mu.Unlock()

	// Run fn outside the map mutex so followers can register while
	// the leader is signing.
	resp := fn()
	call.resp = resp
	close(call.done)

	// Drop the entry — the signRequestCache handles cross-request
	// memoisation. We only deduplicate IN-FLIGHT concurrent calls.
	s.mu.Lock()
	delete(s.calls, key)
	s.mu.Unlock()

	return resp, resp != nil
}
