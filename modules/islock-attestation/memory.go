package islock_attestation

import (
	"sync"
	"time"
)

// IsLockMemory is a validator's bounded, FIFO-evicting cache of recently
// observed Dash InstantSend locks. Populated from dashd ZMQ rawtxlock
// events (NEVER from rawtx — only IS-locked txs belong here, see spec
// §5.6 rev 3 fix).
//
// Concurrent use: safe (mutex-guarded). Designed for a ~1-100 IS-lock/sec
// observation rate on testnet/mainnet — memory pressure is negligible.
//
// FIFO eviction (not LRU): if the cache is full, the oldest-observed
// entry is evicted regardless of how recently it was queried. Prevents
// an attacker from extending the lifetime of an entry by repeatedly
// requesting attestations against it.
type IsLockMemory struct {
	mu      sync.Mutex
	entries map[string]*memoryEntry // txid -> entry
	// fifo is the txids in observation order. fifo[0] is the oldest.
	fifo []string
	// now returns the current time; abstracted for testability.
	now func() time.Time
	// maxEntries caps the cache size. Defaults to MaxIsLockMemoryEntries.
	maxEntries int
	// ttl bounds how long an entry stays valid before pruning. Defaults
	// to IsLockMemoryTTLSeconds.
	ttl time.Duration
}

type memoryEntry struct {
	rawTxHex   string
	rawTxHash  []byte // sha256d for fast lookup match
	observedAt time.Time
}

// NewIsLockMemory builds an empty memory with the default capacity + TTL.
func NewIsLockMemory() *IsLockMemory {
	return &IsLockMemory{
		entries:    make(map[string]*memoryEntry),
		fifo:       make([]string, 0, 1024),
		now:        time.Now,
		maxEntries: MaxIsLockMemoryEntries,
		ttl:        time.Duration(IsLockMemoryTTLSeconds) * time.Second,
	}
}

// WithMaxEntries returns m, configured to use the given cap. Test-only.
func (m *IsLockMemory) WithMaxEntries(n int) *IsLockMemory {
	m.maxEntries = n
	return m
}

// WithTTL returns m, configured to use the given TTL. Test-only.
func (m *IsLockMemory) WithTTL(d time.Duration) *IsLockMemory {
	m.ttl = d
	return m
}

// WithNowFunc replaces the time source for deterministic tests.
func (m *IsLockMemory) WithNowFunc(now func() time.Time) *IsLockMemory {
	m.now = now
	return m
}

// Observe records that this validator saw an IS-lock for txid with the
// given raw tx bytes. Called from the validator's dashd ZMQ rawtxlock
// subscriber. Idempotent: re-observing the same txid updates nothing.
func (m *IsLockMemory) Observe(txid, rawTxHex string, rawTxHash []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if _, exists := m.entries[txid]; exists {
		// Idempotent — keep original observation time so FIFO eviction
		// works on truly oldest entries, not most-recently-attested ones.
		return
	}

	// Evict if at capacity. FIFO by observation order.
	if len(m.entries) >= m.maxEntries {
		oldest := m.fifo[0]
		delete(m.entries, oldest)
		m.fifo = m.fifo[1:]
	}

	m.entries[txid] = &memoryEntry{
		rawTxHex:   rawTxHex,
		rawTxHash:  rawTxHash,
		observedAt: m.now(),
	}
	m.fifo = append(m.fifo, txid)
}

// Lookup retrieves an entry for txid IF it exists and hasn't expired.
// Returns (entry, true) on hit, (nil, false) on miss/expiry. Side effect:
// expired entries are pruned during lookup.
func (m *IsLockMemory) Lookup(txid string) (rawTxHex string, rawTxHash []byte, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	entry, exists := m.entries[txid]
	if !exists {
		return "", nil, false
	}
	if m.now().Sub(entry.observedAt) > m.ttl {
		m.evictByTxid(txid)
		return "", nil, false
	}
	return entry.rawTxHex, entry.rawTxHash, true
}

// Prune removes all entries older than the TTL. Idempotent; safe to call
// periodically (e.g. every minute from a janitor goroutine).
func (m *IsLockMemory) Prune() int {
	m.mu.Lock()
	defer m.mu.Unlock()

	cutoff := m.now().Add(-m.ttl)
	removed := 0
	// Walk fifo from oldest; stop at the first not-yet-expired entry
	// (fifo is in observedAt order so we can short-circuit).
	for len(m.fifo) > 0 {
		head := m.fifo[0]
		entry, exists := m.entries[head]
		if !exists {
			// Already evicted elsewhere — keep going.
			m.fifo = m.fifo[1:]
			continue
		}
		if entry.observedAt.After(cutoff) {
			break // rest of fifo is younger
		}
		delete(m.entries, head)
		m.fifo = m.fifo[1:]
		removed++
	}
	return removed
}

// Len returns the current entry count. Thread-safe.
func (m *IsLockMemory) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// evictByTxid removes a specific entry from both maps. Caller holds mu.
func (m *IsLockMemory) evictByTxid(txid string) {
	delete(m.entries, txid)
	for i, id := range m.fifo {
		if id == txid {
			m.fifo = append(m.fifo[:i], m.fifo[i+1:]...)
			return
		}
	}
}
