package islock_attestation

import (
	"sync"
	"time"
)

// signRequestCache memoises validator-side BLS signatures across
// rebroadcasts of the SAME attestation request. The IS service's
// rebroadcast loop (cmd/is-service/orchestrator.go, audit R16) publishes
// each request up to 3 times within the 15s collect window; without
// dedupe each validator would re-sign on every rebroadcast — three BLS
// signs per legitimate session, and up to ~100/min per malicious peer
// at the validator-side gossip rate limit.
//
// Cache key: (TxId, RawTxHashHex, InstructionHashHex, Epoch). ChainID
// is constant per process so it's not part of the key. The cached
// value is the full IsLockAttestationResponse (sig + pubkey + DID +
// epoch) so a rebroadcast replays the exact bytes without recomputing
// the signature.
//
// TTL = 60s, capacity = 1000 entries with FIFO eviction. The 60s TTL
// is comfortably above R16's 15s collect window (so the full
// rebroadcast set lands within one cache lifetime) and short enough
// that stale entries don't accumulate. Capacity 1000 is ~10× the
// validator-side per-peer rate limit (100/min) so a single attacker
// can't evict legitimate entries.
//
// Audit R17-SEC-dash-rebroadcast-validator-no-per-txid-sign-dedupe.
type signRequestCache struct {
	mu      sync.Mutex
	entries map[string]*signCacheEntry
	// fifo holds keys in insertion order; head is oldest. Eviction
	// pops the head until len(entries) <= capacity.
	fifo []string
	// now is the time source; abstracted for deterministic tests.
	now func() time.Time
}

type signCacheEntry struct {
	resp     *IsLockAttestationResponse
	storedAt time.Time
}

const (
	signCacheTTL      = 60 * time.Second
	signCacheCapacity = 1000
)

func newSignRequestCache() *signRequestCache {
	return &signRequestCache{
		entries: make(map[string]*signCacheEntry, 64),
		fifo:    make([]string, 0, 64),
		now:     time.Now,
	}
}

// signCacheKey derives the cache key for a request. Concatenates the
// four binding fields with NUL separators so distinct field
// combinations can't collide via string overlap.
func signCacheKey(req IsLockAttestationRequest) string {
	// Pre-allocate to avoid per-call growth; the 4 fields together are
	// always ~200 bytes (32-hex txid + 64-hex two hashes + ~6 bytes
	// epoch + 3 separator bytes).
	var b []byte
	b = append(b, req.TxId...)
	b = append(b, 0)
	b = append(b, req.RawTxHashHex...)
	b = append(b, 0)
	b = append(b, req.InstructionHashHex...)
	b = append(b, 0)
	// Epoch is uint64; use a compact decimal repr (varying length is
	// fine since the field is NUL-terminated by the absence of a
	// following separator — last field).
	b = appendUint64(b, req.Epoch)
	return string(b)
}

func appendUint64(b []byte, n uint64) []byte {
	if n == 0 {
		return append(b, '0')
	}
	// Reverse-build the decimal repr.
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	return append(b, tmp[i:]...)
}

// get returns the cached response for key + true if present + not
// expired. Misses (or expirations) clear the entry and return false.
func (c *signRequestCache) get(key string) (*IsLockAttestationResponse, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	if c.now().Sub(e.storedAt) > signCacheTTL {
		// Expired. Remove from the map; leave fifo alone — eviction
		// will catch it later, and removing from a slice middle is O(n)
		// which we don't want on the hot path.
		delete(c.entries, key)
		return nil, false
	}
	return e.resp, true
}

// put stores resp under key. Idempotent: repeated puts of the same
// key update the entry in place (no double-FIFO push). Evicts the
// oldest entries when capacity is exceeded.
func (c *signRequestCache) put(key string, resp *IsLockAttestationResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, dup := c.entries[key]; dup {
		// Idempotent update; refresh storedAt but don't push fifo.
		c.entries[key] = &signCacheEntry{resp: resp, storedAt: c.now()}
		return
	}
	c.entries[key] = &signCacheEntry{resp: resp, storedAt: c.now()}
	c.fifo = append(c.fifo, key)
	for len(c.entries) > signCacheCapacity {
		oldest := c.fifo[0]
		c.fifo = c.fifo[1:]
		delete(c.entries, oldest)
	}
}
