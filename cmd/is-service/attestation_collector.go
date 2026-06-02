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
	awaiters map[string]chan islock.IsLockAttestationResponse
}

func newAttestationCollector() *attestationCollector {
	return &attestationCollector{
		awaiters: make(map[string]chan islock.IsLockAttestationResponse),
	}
}

// Await registers a per-txid channel and returns it. Caller is responsible
// for calling Cancel(txid) when done. Buffer sizes the channel to avoid
// blocking Deliver under load.
func (c *attestationCollector) Await(txid string, buffer int) <-chan islock.IsLockAttestationResponse {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.awaiters[txid]; ok {
		return existing
	}
	ch := make(chan islock.IsLockAttestationResponse, buffer)
	c.awaiters[txid] = ch
	return ch
}

// Cancel removes the awaiter for txid. Idempotent — safe to call from a
// goroutine that doesn't know whether quorum was reached or the deadline
// fired.
func (c *attestationCollector) Cancel(txid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.awaiters[txid]; ok {
		delete(c.awaiters, txid)
		close(ch)
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
	ch, ok := c.awaiters[resp.TxId]
	if !ok {
		return
	}
	select {
	case ch <- resp:
	default:
		// Buffer full — quorum has clearly been over-met; drop silently.
	}
}

// Collect blocks until either threshold responses arrive or ctx is done.
// Returns the collected set. If ctx expires before threshold, returns
// whatever was collected (caller decides whether that's enough).
//
// Dedupes by ValidatorDID — a single validator signing twice (network
// glitch causing resend) counts once.
func (c *attestationCollector) Collect(
	ctx context.Context,
	txid string,
	threshold int,
	timeout time.Duration,
) []islock.IsLockAttestationResponse {
	ch := c.Await(txid, threshold*2) // double buffer for over-quorum tolerance
	defer c.Cancel(txid)

	deadline := time.NewTimer(timeout)
	defer deadline.Stop()

	seen := make(map[string]struct{}, threshold)
	out := make([]islock.IsLockAttestationResponse, 0, threshold)

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
			if len(out) >= threshold {
				return out
			}
		case <-deadline.C:
			return out
		case <-ctx.Done():
			return out
		}
	}
}
