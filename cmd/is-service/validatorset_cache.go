package main

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// validatorSetCache wraps SubmitterL2.FetchValidatorSet with a
// per-epoch TTL cache. Round-4 audit R4-001 motivates the wiring;
// caching avoids hitting GraphQL on every Drive() while still
// reflecting admin-side updates within ttl.
//
// Concurrency: many sessions can request the same epoch in parallel;
// in-flight requests for the same epoch are coalesced via a per-epoch
// singleflight token so we issue one upstream fetch per epoch per
// TTL window.
//
// Round-5 audit R5-006/R5-CORRECT-01/R5-CORR-02: a transient fetch
// error MUST NOT evict the previous good entry, otherwise a single
// GraphQL flap silently disables the R3-07 DID↔pubkey cross-check
// across every Drive() until the next successful fetch. The fix
// preserves the prior value during the load window and on failure
// returns it (with refreshed expiresAt for short-term reuse) so the
// orchestrator keeps verifying against the last-known good set.
type validatorSetCache struct {
	fetcher    *SubmitterL2
	contractID string
	ttl        time.Duration

	mu      sync.Mutex
	entries map[uint64]*validatorSetEntry
}

type validatorSetEntry struct {
	value     map[string]string
	expiresAt time.Time
	loading   chan struct{} // non-nil while a load is in flight
}

func newValidatorSetCache(fetcher *SubmitterL2, contractID string, ttl time.Duration) *validatorSetCache {
	return &validatorSetCache{
		fetcher:    fetcher,
		contractID: contractID,
		ttl:        ttl,
		entries:    make(map[uint64]*validatorSetEntry),
	}
}

// Get returns the cached or freshly-fetched validator set for the
// epoch. Returns nil (no error path — the orchestrator treats nil as
// "fall back to raw-sig verify") on first-call upstream error;
// subsequent transient errors fall back to the last-known good value
// so the R3-07 gate stays active across short GraphQL flaps.
func (c *validatorSetCache) Get(ctx context.Context, epoch uint64) map[string]string {
	c.mu.Lock()
	entry, ok := c.entries[epoch]
	if ok && entry.loading == nil && time.Now().Before(entry.expiresAt) {
		v := entry.value
		c.mu.Unlock()
		return v
	}
	if ok && entry.loading != nil {
		// Someone else is loading; release the lock so they can
		// finish and wait for them. Return whatever value is on
		// disk after they finish — could be the prior value (on
		// fetch error) or the new fetched value.
		loading := entry.loading
		c.mu.Unlock()
		select {
		case <-loading:
		case <-ctx.Done():
			return nil
		}
		c.mu.Lock()
		entry = c.entries[epoch]
		var v map[string]string
		if entry != nil {
			v = entry.value
		}
		c.mu.Unlock()
		return v
	}
	loadCh := make(chan struct{})
	// Preserve any existing value during the load window — on error
	// we want it to remain readable, and on success we'll overwrite.
	var priorValue map[string]string
	if entry != nil {
		priorValue = entry.value
	}
	c.entries[epoch] = &validatorSetEntry{
		value:   priorValue,
		loading: loadCh,
	}
	c.mu.Unlock()

	fetched, err := c.fetcher.FetchValidatorSet(ctx, c.contractID, epoch)
	c.mu.Lock()
	defer c.mu.Unlock()
	close(loadCh)
	if err != nil {
		slog.Warn("validator-set cache: upstream fetch failed",
			"epoch", epoch, "err", err, "stalePreserved", priorValue != nil)
		// Don't evict the previous-good entry — round-5 R5-006.
		// Keep the prior value but mark it expired-soon so the
		// next caller retries the upstream fetch. A short
		// retry-delay (1/4 ttl) prevents tight retry loops while
		// allowing recovery within seconds of a transient flap.
		retryDelay := c.ttl / 4
		if retryDelay < 5*time.Second {
			retryDelay = 5 * time.Second
		}
		c.entries[epoch] = &validatorSetEntry{
			value:     priorValue,
			expiresAt: time.Now().Add(retryDelay),
		}
		return priorValue
	}
	c.entries[epoch] = &validatorSetEntry{
		value:     fetched,
		expiresAt: time.Now().Add(c.ttl),
	}
	return fetched
}

// GC drops cache entries whose expiresAt is older than maxAge.
// Intended to be called from a janitor goroutine to bound memory
// when the orchestrator queries many distinct epochs over time.
// Round-5 audit R5-OP-05.
func (c *validatorSetCache) GC(maxAge time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	removed := 0
	for epoch, entry := range c.entries {
		if entry.loading != nil {
			continue
		}
		if now.Sub(entry.expiresAt) > maxAge {
			delete(c.entries, epoch)
			removed++
		}
	}
	return removed
}
