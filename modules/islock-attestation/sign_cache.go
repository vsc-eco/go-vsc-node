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
// Amplification bypass (audit R18-SEC-cache-amplification-by-
// varying-instruction-hash + R19-OPS-cache-amplification-runtime-
// defense-undersized-vs-multi-peer): an attacker can defeat this
// dedupe by VARYING any cache-key field per request. The validator
// pays the BLS-sign cost on every miss.
//
// Single-peer threat model: the per-peer requestRateLimiter
// (100 msg/min in p2p.go) caps any one peer at ~1.67 msg/sec,
// which is comfortably below the BLS-sign throughput. Capacity=1000
// is ~10× the single-peer rate so legitimate hits aren't displaced.
//
// Multi-peer threat model (R19): with N malicious peers each
// spending their full per-peer budget, total cache-miss rate scales
// at N × 100/min. At N=10 the cache fills with junk in 60s; at
// N=100 in 6s. The cache alone CANNOT bound this — the relevant
// defenses are upstream:
//   - gossipsub mesh degree (D=6 default) caps the FAN-OUT a single
//     attacker can reach; an attacker has to control multiple peers
//     to multiply.
//   - IS-service-instance count: legitimate rebroadcasts arrive from
//     N IS-service instances (typically 1 per deployment), so the
//     "100 malicious peers all gossiping" model assumes a much-larger
//     compromise of the validator-set gossip topology. NOTE the
//     gossipsub topic itself has NO peer-allowlist enforcement at
//     this layer — anyone on the libp2p mesh can publish — so this
//     is an observational property of normal deployments, not a
//     hard membership filter. Audit R20-SEC-sign-cache-godoc-
//     validator-set-membership-claim-is-not-enforced corrected the
//     prior phrasing that implied an enforced filter.
//   - BLS-sign latency (~tens of ms) caps the validator's own
//     throughput at ~100 signs/sec/core — eviction churn is the
//     observable, not CPU exhaustion.
//
// In the realistic 1-5 peer threat, capacity=1000 + 60s TTL is
// over-provisioned. For larger compromises an LRU layer (rather
// than FIFO) over recent cache HITS would protect legitimate
// entries, but that's not in scope here.
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

// Cache tuning constants. Currently fixed; audit R18-OPS-sign-cache-
// no-metrics-or-knob-impacts-tuning flagged the lack of observability
// (no hit/miss/eviction counters) + no operator knob. Both are
// deferred: the cache is small enough (~50KB worst-case at capacity)
// and the trade-offs are well-understood from the audit reasoning,
// so a metrics endpoint + env-var overrides can land in a dedicated
// observability ticket rather than a fix-cluster sweep.
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
//
// Size: Dash txid is 64 hex chars; rawTxHash + instructionHash are
// 64 hex chars each (sha256d / sha256 outputs); epoch is up to
// ~20 decimal digits; plus 3 NUL separators ≈ 215 bytes typical.
// Audit R18-CONS-sign-cache-key-comment-claims-pre-allocation-and-
// 32-hex-txid: the prior comment claimed "pre-allocate" + "32-hex
// txid"; neither was right. Append-from-nil-slice is fine here
// (Go grows the slice in O(1) amortised), and the txid hex length
// is 64, not 32.
func signCacheKey(req IsLockAttestationRequest) string {
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
		// Expired. Remove from the map AND splice the key out of fifo.
		//
		// Audit R18-SEC-sign-cache-fifo-duplicate-on-expire-reput +
		// R18-CORR-sign-cache-fifo-dup-on-expired-reput +
		// R18-OPS-sign-cache-fifo-grows-unbounded-after-ttl-reinsertion:
		// the pre-R18 code intentionally left fifo alone with the
		// comment "eviction will catch it later". The pathology was: a
		// subsequent put(key) — common because the same (TxId,
		// rawTxHash, instructionHash, epoch) tuple is the exact case
		// the dedupe was designed to memoise — sees entries[key] gone
		// (deleted just now), falls through the dup-check, and appends
		// ANOTHER fifo entry for the same key. fifo now has two slots
		// for one entry. Later eviction pops the STALE fifo slot and
		// calls delete(entries, key) — destroying the FRESH entry
		// that was supposed to be cached for 60s. The dedupe quietly
		// failed and the validator re-signed. fifo also grew without
		// bound under sustained expire/reput cycles.
		//
		// Cost: O(n) splice on the expire branch. n ≤ signCacheCapacity
		// (1000); per-call cost is microseconds. Expires are not the
		// hot path so the trade is fine.
		delete(c.entries, key)
		for i, k := range c.fifo {
			if k == key {
				c.fifo = append(c.fifo[:i], c.fifo[i+1:]...)
				break
			}
		}
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
