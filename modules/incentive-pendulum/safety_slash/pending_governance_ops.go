package safetyslash

import "sync"

// PendingGovernanceOps is the witness-local pool of governance ops
// awaiting inclusion in the next block this node leads. The submission
// path is intentionally out of scope — operators (RPC handler, admin
// CLI, future DAO contract action) call EnqueueRestitutionClaim /
// EnqueueSlashReverse to populate the pool, and the block producer
// drains it during GenerateBlock for the slots this node leads.
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
	// EnqueueRestitutionClaim adds a claim record to the pool. Returns
	// false if the record fails basic structural validation (which
	// would never be accepted on-chain anyway). Idempotent on
	// (rec.ClaimID): re-enqueueing the same id replaces the existing
	// entry rather than duplicating it, so off-chain retries don't
	// inflate the pool.
	EnqueueRestitutionClaim(rec RestitutionClaimRecord) bool

	// EnqueueSlashReverse adds a reverse record to the pool. Returns
	// false if the record fails basic structural validation.
	// Idempotent on (rec.SlashTxID + rec.EvidenceKind + rec.Action):
	// re-enqueueing the same governance request replaces the existing
	// entry.
	EnqueueSlashReverse(rec SafetySlashReverseRecord) bool

	// DrainRestitutionClaims removes and returns up to `max` claims in
	// FIFO order. Called by the block producer when composing a
	// proposal. Pass 0 to fetch every pending claim.
	DrainRestitutionClaims(max int) []RestitutionClaimRecord

	// DrainSlashReverses removes and returns up to `max` reverses in
	// FIFO order.
	DrainSlashReverses(max int) []SafetySlashReverseRecord

	// PendingCounts returns the current sizes of both queues. Useful
	// for metrics / RPC observability.
	PendingCounts() (claims, reverses int)
}

// MemoryPendingGovernanceOps is the default in-memory implementation.
// Thread-safe; safe for concurrent submission and drain.
type MemoryPendingGovernanceOps struct {
	mu       sync.Mutex
	claims   []RestitutionClaimRecord
	reverses []SafetySlashReverseRecord
}

// NewMemoryPendingGovernanceOps returns an empty pool ready for use.
func NewMemoryPendingGovernanceOps() *MemoryPendingGovernanceOps {
	return &MemoryPendingGovernanceOps{
		claims:   make([]RestitutionClaimRecord, 0),
		reverses: make([]SafetySlashReverseRecord, 0),
	}
}

// validClaimShape gates entries before they enter the pool. This is a
// cheap pre-filter; the chain-op apply path runs the authoritative
// checks at apply time. Mirrors the required-field validation in
// applyRestitutionClaim so we don't waste pool slots on entries that
// will never apply.
func validClaimShape(r RestitutionClaimRecord) bool {
	r = r.Normalize()
	if r.ClaimID == "" || r.VictimAccount == "" ||
		r.SlashTxID == "" || r.SlashedAccount == "" || r.EvidenceKind == "" {
		return false
	}
	if r.LossHive <= 0 {
		return false
	}
	return true
}

// validReverseShape mirrors required-field validation in
// applySafetySlashReverse for the same reason as validClaimShape.
func validReverseShape(r SafetySlashReverseRecord) bool {
	r = r.Normalize()
	if r.SlashTxID == "" || r.EvidenceKind == "" || r.SlashedAccount == "" {
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

func (p *MemoryPendingGovernanceOps) EnqueueRestitutionClaim(rec RestitutionClaimRecord) bool {
	rec = rec.Normalize()
	if !validClaimShape(rec) {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, existing := range p.claims {
		if existing.ClaimID == rec.ClaimID {
			// Idempotent: replace the existing entry so off-chain
			// retries with the same ClaimID do not inflate the pool.
			p.claims[i] = rec
			return true
		}
	}
	p.claims = append(p.claims, rec)
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
	p.reverses = append(p.reverses, rec)
	return true
}

func (p *MemoryPendingGovernanceOps) DrainRestitutionClaims(max int) []RestitutionClaimRecord {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.claims) == 0 {
		return nil
	}
	n := len(p.claims)
	if max > 0 && max < n {
		n = max
	}
	out := make([]RestitutionClaimRecord, n)
	copy(out, p.claims[:n])
	p.claims = append(p.claims[:0], p.claims[n:]...)
	return out
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

func (p *MemoryPendingGovernanceOps) PendingCounts() (claims, reverses int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.claims), len(p.reverses)
}

var _ PendingGovernanceOps = (*MemoryPendingGovernanceOps)(nil)
