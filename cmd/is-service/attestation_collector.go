package main

import (
	"context"
	"sync"
	"time"

	islock "vsc-node/modules/islock-attestation"
)

// attestationCollector gates p2p responses into per-txid channels so the
// orchestrator can collect N-of-M responses with a timeout.
//
// The IS service registers itself as the onResponse callback when
// constructing the islock_attestation.Service. Inbound responses route
// through Deliver, which fans them out to the channel registered for
// that txid (or drops the response if no awaiter — late responses).
//
// Per-txid bounded fan-in: caller passes in the channel buffer (the
// expected number of validators). If buffer fills we drop further
// responses — quorum's already over-met.
type attestationCollector struct {
	mu       sync.Mutex
	awaiters map[string]*awaiter
}

// awaiter is the refcounted bag around a per-txid response channel.
// Multiple Drive goroutines awaiting the same txid (which happens when
// the watcher cross-product fan-out fires) all share the channel; only
// the LAST Cancel actually closes it. Audit
// `attestation-collector-await-shares-channel`.
type awaiter struct {
	ch       chan islock.IsLockAttestationResponse
	refcount int
}

func newAttestationCollector() *attestationCollector {
	return &attestationCollector{
		awaiters: make(map[string]*awaiter),
	}
}

// Await registers a per-txid channel and returns it. Caller is
// responsible for calling Cancel(txid) when done. Buffer sizes the
// channel to avoid blocking Deliver under load. Multiple Awaits for
// the same txid share the same channel; refcounted so Cancel only
// closes when the last awaiter releases.
func (c *attestationCollector) Await(txid string, buffer int) <-chan islock.IsLockAttestationResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.awaiters[txid]; ok {
		existing.refcount++
		return existing.ch
	}
	a := &awaiter{
		ch:       make(chan islock.IsLockAttestationResponse, buffer),
		refcount: 1,
	}
	c.awaiters[txid] = a
	return a.ch
}

// Cancel decrements the refcount; closes the channel + removes the
// entry only when the refcount drops to zero. Idempotent on
// nonexistent txid.
func (c *attestationCollector) Cancel(txid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.awaiters[txid]
	if !ok {
		return
	}
	a.refcount--
	if a.refcount <= 0 {
		delete(c.awaiters, txid)
		close(a.ch)
	}
}

// Deliver is the p2p-side callback. Called by the islock_attestation
// service when a response arrives on the gossip topic. Non-blocking:
// drops the response if the awaiter's buffer is full or no awaiter
// exists (late delivery).
//
// Holds c.mu for the duration of the channel send so concurrent Cancel
// can't close-during-send. The select uses default so the lock is held
// only briefly even when the buffer is full.
func (c *attestationCollector) Deliver(resp islock.IsLockAttestationResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.awaiters[resp.TxId]
	if !ok {
		return
	}
	select {
	case a.ch <- resp:
	default:
		// Buffer full — quorum has clearly been over-met; drop silently.
	}
}

// Collect blocks until either threshold VERIFIED responses arrive or
// ctx/timeout fires. Returns the FULL collected set (verified +
// unverified) for caller-side diagnostics.
//
// `verifier`, when non-nil, gates which responses count toward
// threshold. Use this to keep collecting past attacker / mesh-race
// noise: with quorumThreshold=1 and the legacy threshold-on-raw-count
// behaviour, a single response from a non-registered validator (or
// a peer that signed with the wrong DID) would fill the threshold,
// the collector returned immediately, and the orchestrator's per-sig
// verify dropped it — ATTESTATION_TIMEOUT with verified=0 collected=1.
//
// Audit `attestation-collector-counts-unverified-toward-threshold`
// (op=call devnet test E2E, 2026-06-05): the IS service's gossipsub
// mesh propagates the attestation request to every connected magi
// witness, but only a subset is registered in setValidatorSet (often
// just magi-1 in test setups, or the active committee in production).
// Witnesses not in the registered set sign with their own BLS keys
// and respond first ~50% of the time (race condition on mesh
// propagation order). Pre-fix: the FIRST response — regardless of
// validator-set membership — terminated Collect, then was rejected
// post-collect. Post-fix: the verifier closure runs per-response;
// responses that fail validator-set + per-sig verify don't count
// toward threshold, so Collect keeps waiting until enough VERIFIED
// responses arrive (or timeout). Unverified responses are still
// appended to the return so the orchestrator's "no responder matched
// expected set" diagnostic still surfaces.
//
// When verifier is nil, falls back to legacy threshold-on-raw-count
// (used by unit tests + future callers that handle verify themselves).
//
// Dedupes by ValidatorDID — a single validator signing twice (network
// glitch causing resend) counts once. Buffer is sized at threshold*2
// to tolerate over-quorum; with the verifier-aware variant a larger
// flood is possible (one unverified response per non-registered peer
// in the mesh) so callers may want a wider buffer in the future.
func (c *attestationCollector) Collect(
	ctx context.Context,
	txid string,
	threshold int,
	timeout time.Duration,
	verifier func(islock.IsLockAttestationResponse) bool,
) []islock.IsLockAttestationResponse {
	// Buffer sizing: with verifier-aware collection there may be more
	// raw responses than verified ones (one per non-registered mesh
	// peer that also signs). Allow up to 8× threshold so a typical
	// 5-witness mesh can flood without dropping at Deliver. The
	// orchestrator caller passes threshold=1..few; even 8× is small.
	buffer := threshold * 2
	if verifier != nil {
		buffer = threshold * 8
	}
	ch := c.Await(txid, buffer)
	defer c.Cancel(txid)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	seen := make(map[string]struct{}, threshold)
	out := make([]islock.IsLockAttestationResponse, 0, threshold)
	verifiedCount := 0

	for {
		select {
		case resp, ok := <-ch:
			if !ok {
				return out
			}
			if _, dup := seen[resp.ValidatorDID]; dup {
				continue
			}
			seen[resp.ValidatorDID] = struct{}{}
			out = append(out, resp)
			countsTowardThreshold := true
			if verifier != nil {
				countsTowardThreshold = verifier(resp)
			}
			if countsTowardThreshold {
				verifiedCount++
				if verifiedCount >= threshold {
					return out
				}
			}
		case <-deadline.C:
			return out
		case <-ctx.Done():
			return out
		}
	}
}
