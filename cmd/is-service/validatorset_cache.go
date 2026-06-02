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
	loading   chan struct{} // closed when an in-flight load finishes
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
// "fall back to raw-sig verify") on any upstream error; failures are
// logged at WARN level.
//
// The cache is not poisoned by errors: a fetch that fails leaves the
// previous entry (if any) intact and just expires the loading token
// so the next caller retries. This avoids a stuck-empty cache when
// the L2 GraphQL endpoint flaps.
func (c *validatorSetCache) Get(ctx context.Context, epoch uint64) map[string]string {
	c.mu.Lock()
	entry, ok := c.entries[epoch]
	if ok && entry.loading == nil && time.Now().Before(entry.expiresAt) {
		v := entry.value
		c.mu.Unlock()
		return v
	}
	if ok && entry.loading != nil {
		// Someone else is loading; wait for them under the cache
		// lock released so they can finish.
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
	c.entries[epoch] = &validatorSetEntry{
		loading: loadCh,
	}
	c.mu.Unlock()

	fetched, err := c.fetcher.FetchValidatorSet(ctx, c.contractID, epoch)
	c.mu.Lock()
	defer c.mu.Unlock()
	close(loadCh)
	if err != nil {
		slog.Warn("validator-set cache: upstream fetch failed",
			"epoch", epoch, "err", err)
		// Don't store the empty result — let the next caller retry.
		delete(c.entries, epoch)
		return nil
	}
	c.entries[epoch] = &validatorSetEntry{
		value:     fetched,
		expiresAt: time.Now().Add(c.ttl),
	}
	return fetched
}
