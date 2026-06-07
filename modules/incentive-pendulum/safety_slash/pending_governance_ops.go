package safetyslash

import (
	"strings"
	"sync"
)

// PendingGovernanceOps is the witness-local pool of governance ops
// awaiting inclusion in the next block this node leads. The submission
// path is intentionally out of scope — operators (RPC handler, admin
// CLI, governance multisig) call EnqueueSlashReverse to populate
// the pool, and the block producer drains it during GenerateBlock for
// the slots this node leads.
//
// Determinism note: the pool is process-local, so different leaders
// see different pools. That is fine — the chain-op apply path
// validates each payload against on-chain state at apply time, so a
// stale / invalid op simply gets dropped. The CID over the carrying
// block is the consensus on which payloads landed; signers fetch the
// bytes from the DataLayer, ratify or refuse, and the BLS aggregate
// finalises the decision. The pool is a *suggestion queue* the
// leader consumes from when composing their proposal.
type PendingGovernanceOps interface {
	// EnqueueSlashReverse adds a reverse record to the pool. Returns
	// false if the record fails basic structural validation.
	// Idempotent on (rec.SlashTxID + rec.EvidenceKind + rec.Action):
	// re-enqueueing the same governance request replaces the existing
	// entry.
	EnqueueSlashReverse(rec SafetySlashReverseRecord) bool

	// DrainSlashReverses removes and returns up to `max` reverses in
	// FIFO order. Called by the block producer when composing a
	// proposal. Pass 0 to fetch every pending reverse.
	DrainSlashReverses(max int) []SafetySlashReverseRecord

	// PendingCounts returns the current size of the reverse queue.
	// Useful for metrics / RPC observability.
	PendingCounts() (reverses int)
}

// MemoryPendingGovernanceOps is the default in-memory implementation.
// Thread-safe; safe for concurrent submission and drain.
type MemoryPendingGovernanceOps struct {
	mu       sync.Mutex
	reverses []SafetySlashReverseRecord
}

// NewMemoryPendingGovernanceOps returns an empty pool ready for use.
func NewMemoryPendingGovernanceOps() *MemoryPendingGovernanceOps {
	return &MemoryPendingGovernanceOps{
		reverses: make([]SafetySlashReverseRecord, 0),
	}
}

// maxPendingReverses bounds the witness-local pool so a flood of governance
// submissions (e.g. from a compromised governance key) cannot exhaust node memory.
// Generous — real wrongful-slash cancellations are rare — but finite. When
// full, EnqueueSlashReverse rejects new entries (existing ones still drain).
const maxPendingReverses = 10_000

// validReverseShape mirrors required-field validation in
// applySafetySlashReverse — a cheap pre-filter so we don't waste pool
// slots on entries that will never apply; the chain-op apply path runs
// the authoritative checks at apply time.
func validReverseShape(r SafetySlashReverseRecord) bool {
	r = r.Normalize()
	if r.SlashTxID == "" || r.EvidenceKind == "" || r.SlashedAccount == "" {
		return false
	}
	// "#" is the ledger row-Id delimiter (rows are keyed
	// "<SlashTxID>#safety_slash#<EvidenceKind>#..."). A "#" in either field
	// could craft an ambiguous/colliding lookup key; no real Hive tx id or
	// evidence kind contains it. The apply path's exact-Id match already makes
	// a collision unexploitable, but reject here as defense-in-depth.
	if strings.Contains(r.SlashTxID, "#") || strings.Contains(r.EvidenceKind, "#") {
		return false
	}
	switch r.Action {
	case ReverseActionCancel, ReverseActionReverse, ReverseActionBoth:
		// ok
	default:
		return false
	}
	return true
}

// reverseDedupKey is the deterministic key used to dedupe reverse
// requests inside the pool. Two governance requests targeting the
// same slash with the same action collapse to one entry — the most
// recent submission wins. (Distinct chain-op tx ids on the carrying
// block side still produce distinct rows in the ledger, so this
// pool-side dedup is purely an off-chain convenience.)
func reverseDedupKey(r SafetySlashReverseRecord) string {
	return r.SlashTxID + "|" + r.EvidenceKind + "|" + r.SlashedAccount + "|" + string(r.Action)
}

func (p *MemoryPendingGovernanceOps) EnqueueSlashReverse(rec SafetySlashReverseRecord) bool {
	rec = rec.Normalize()
	if !validReverseShape(rec) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := reverseDedupKey(rec)
	for i, existing := range p.reverses {
		if reverseDedupKey(existing) == key {
			p.reverses[i] = rec
			return true
		}
	}
	// Bound pool growth (a dedup-evading flood varies the key). Reject once
	// full; queued entries still drain into blocks and free slots.
	if len(p.reverses) >= maxPendingReverses {
		return false
	}
	p.reverses = append(p.reverses, rec)
	return true
}

func (p *MemoryPendingGovernanceOps) DrainSlashReverses(max int) []SafetySlashReverseRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.reverses) == 0 {
		return nil
	}
	n := len(p.reverses)
	if max > 0 && max < n {
		n = max
	}
	out := make([]SafetySlashReverseRecord, n)
	copy(out, p.reverses[:n])
	p.reverses = append(p.reverses[:0], p.reverses[n:]...)
	return out
}

func (p *MemoryPendingGovernanceOps) PendingCounts() (reverses int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.reverses)
}

var _ PendingGovernanceOps = (*MemoryPendingGovernanceOps)(nil)
