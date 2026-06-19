package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	islockinstruction "vsc-node/lib/islock-instruction"
)

// SessionState is the lifecycle phase of an IS-login session. Mirrors the
// state machine in spec §5.7 rev 6.
type SessionState string

const (
	StateWaitingForIS SessionState = "WAITING_FOR_IS"
	StateISObserved   SessionState = "IS_OBSERVED"
	StateAttesting    SessionState = "ATTESTING"
	// StateL2Submitted: orchestrator has handed the mapInstantSendV2
	// tx to the L2 GraphQL endpoint (mempool-accepted) but has not yet
	// confirmed execution. The reconciler polls FetchTransactionStatus
	// until terminal — audit D2-DESIGN-06. SessionToken is NOT minted
	// at this state.
	StateL2Submitted        SessionState = "L2_SUBMITTED"
	StateOnChain            SessionState = "ON_CHAIN"
	StateAttestationTimeout SessionState = "ATTESTATION_TIMEOUT"
	// StateSlowPathPending: the L2 submission landed in the mempool +
	// reconciliation timed out without a terminal status. The tx may
	// still confirm and credit via the contract's slow-path mined-block
	// proof. Audit M8: orchestrator.pending() routes here (vs
	// ForwardFailed) so the frontend can show "still being mined" and a
	// recovery scanner can re-poll. Session.L2TxId + Session.ForwardError
	// carry the human-readable hint.
	StateSlowPathPending SessionState = "SLOW_PATH_PENDING"
	StateForwardFailed   SessionState = "FORWARD_FAILED"
	StateExpired         SessionState = "EXPIRED"
)

// IsTerminal reports whether the state will not advance on its own —
// the session is done from the IS service's perspective.
//
// Round-4 audit R4-SEC-03: StateAttestationTimeout and
// StateSlowPathPending are terminal from the orchestrator's perspective
// (Drive returns after MutateState'ing into them); including them here
// prevents Get() / Prune() from silently overwriting them to
// StateExpired past ExpiresAt and destroying operator visibility into
// validator-mesh degradation.
func (s SessionState) IsTerminal() bool {
	switch s {
	case StateOnChain, StateForwardFailed, StateExpired,
		StateAttestationTimeout, StateSlowPathPending:
		return true
	}
	return false
}

// IsInFlight reports whether the orchestrator still holds the session
// in active processing — between IS-lock observation and a terminal
// state. Get() and Prune() use this to avoid clobbering in-flight
// state past ExpiresAt; the orchestrator is allowed to outrun the
// session TTL on a slow L2 reconcile.
//
// Round-3 audit R3-CSM-02 motivated this carve-out for Prune;
// round-4 audit R4-SEC-01 extended it to Get (which was the other
// half of the same divergence).
func (s SessionState) IsInFlight() bool {
	switch s {
	case StateAttesting, StateL2Submitted:
		return true
	}
	return false
}

// Op identifies the instruction shape the user is paying for. The IS
// service builds the instruction string per the spec's grammar:
//
//	op=auth;sid=<sid>
//	op=call;contract=<id>;method=<m>;args=<base64>;sid=<sid>[;amount=<n>]
type Op string

const (
	OpAuth Op = "auth"
	OpCall Op = "call"
)

// Session is one IS-login attempt's lifecycle: from address handed to the
// user, through IS-lock observation, through quorum attestation, through
// on-chain credit.
type Session struct {
	Sid              string
	Op               Op
	Instruction      string
	DepositAddress   string
	RequiredAmount   int64 // in duffs (10^-8 DASH)
	AddressSignature string
	CreatedAt        time.Time
	ExpiresAt        time.Time
	State            SessionState
	DashTxId         string // populated on IS_OBSERVED
	SenderAddress    string // populated on IS_OBSERVED — DashL1 address that paid
	OnChainAt        *time.Time
	L2TxId           string // populated on ON_CHAIN — the mapInstantSendV2 L2 tx CID
	SessionToken     string // populated on ON_CHAIN + FORWARD_OK
	ForwardError     string // populated on FORWARD_FAILED
}

// SessionStore is an in-memory store of active sessions. Concurrent-safe.
// Sessions older than the configured maxAge are pruned on access; for high
// volume a janitor goroutine should call Prune periodically.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]*Session
	maxAge   time.Duration
	now      func() time.Time
}

// NewSessionStore builds an empty store. maxAge is the duration after
// CreatedAt past which sessions are considered expired and pruned.
func NewSessionStore(maxAge time.Duration) *SessionStore {
	return &SessionStore{
		sessions: make(map[string]*Session),
		maxAge:   maxAge,
		now:      time.Now,
	}
}

// WithNowFunc replaces the time source for deterministic tests.
func (s *SessionStore) WithNowFunc(now func() time.Time) *SessionStore {
	s.now = now
	return s
}

// Get fetches a session by sid. Returns (nil, false) on miss.
// Side effect: non-terminal, non-in-flight sessions past ExpiresAt
// are marked StateExpired so a polling client can observe the
// transition; the entry is reclaimed on the next Prune cycle.
//
// Round-4 audit R4-SEC-01: in-flight sessions (Attesting / L2Submitted)
// are NEVER mutated to Expired here — reconcileL2 may outrun the TTL
// on a slow L2, and clobbering state on read would defeat the Prune
// carve-out from the other side.
//
// Round-4 audit R4-SEC-03: terminal states (including
// AttestationTimeout / SlowPathPending) are similarly preserved so
// /healthz CountByState can surface mesh-degradation signal rather
// than seeing every late-poll session reclassified as Expired.
func (s *SessionStore) Get(sid string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sid]
	if !ok {
		return nil, false
	}
	if s.now().After(sess.ExpiresAt) && !sess.State.IsTerminal() && !sess.State.IsInFlight() {
		sess.State = StateExpired
	}
	return sess, true
}

// MaxSessions is the global cap on concurrent sessions. Beyond this,
// new sessions are rejected to bound memory + watcher entries — audit
// `dashd-watcher-leaks-after-session-terminal`.
const MaxSessions = 50_000

// ErrSessionStoreFull is returned by Put when the store is at MaxSessions.
var ErrSessionStoreFull = errors.New("session store full — global cap reached")

// ErrSidAlreadyExists is returned by PutNew when an entry with the
// given sid already exists. Round-3 audit R3-005 — handler used to do
// a Get-then-Put TOCTOU which let two concurrent /session/start with
// the same sid race past the existence check and silently clobber.
var ErrSidAlreadyExists = errors.New("sid already exists")

// PutNew inserts a fresh session atomically. Returns ErrSidAlreadyExists
// if any entry for sess.Sid already exists (regardless of state).
// Returns ErrSessionStoreFull when the global cap is reached.
//
// Callers MUST prefer PutNew over Put for /session/start to close the
// concurrent-same-sid race (R3-005). Put remains for state-machine
// transitions where overwrite is intended.
func (s *SessionStore) PutNew(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[sess.Sid]; exists {
		return ErrSidAlreadyExists
	}
	if len(s.sessions) >= MaxSessions {
		return ErrSessionStoreFull
	}
	s.sessions[sess.Sid] = sess
	return nil
}

// Put stores or overwrites a session. Returns ErrSessionStoreFull when
// the global cap is reached for a NEW session id (overwriting an
// existing sid always succeeds).
func (s *SessionStore) Put(sess *Session) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[sess.Sid]; !exists && len(s.sessions) >= MaxSessions {
		return ErrSessionStoreFull
	}
	s.sessions[sess.Sid] = sess
	return nil
}

// CountByState returns a map of {state → count} across all live
// sessions. Used by /healthz for operator visibility into where
// sessions are getting stuck. Round-3 audit OP-004.
func (s *SessionStore) CountByState() map[SessionState]int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make(map[SessionState]int)
	for _, sess := range s.sessions {
		out[sess.State]++
	}
	return out
}

// MutateState atomically applies a state transition. The fn receives a
// pointer to the current session (under the store's lock) and may mutate
// any field. Returns false if no such session exists.
func (s *SessionStore) MutateState(sid string, fn func(*Session)) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sid]
	if !ok {
		return false
	}
	fn(sess)
	return true
}

// Prune removes terminal-state and expired sessions. Returns the
// deposit addresses of pruned sessions so the caller can Unwatch
// them.
//
// Round-2 audit R2-N3: TTL-expired sessions were leaking watcher map
// entries because the prune janitor only touched SessionStore.
//
// Round-3 audit R3-CSM-02: Prune now skips sessions in non-terminal
// in-flight states (Attesting / L2Submitted) even past ExpiresAt. The
// reconcileL2 polling loop can outrun the 30-min TTL on a slow L2;
// deleting mid-reconcile produces the L2-credited-but-session-lost
// divergence the reconciler was designed to prevent.
func (s *SessionStore) Prune() (removed int, prunedDepositAddrs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// Round-5 audit R5-CORR-01: terminal-but-not-Expired sessions
	// stay around long enough that /healthz CountByState surfaces
	// validator-mesh degradation (R4-SEC-03 promise). We need a
	// terminal-retention window distinct from the WaitingForIS TTL
	// so explicit attestation timeouts/forward-failures don't
	// silently roll into the generic Expired bucket on the next
	// prune tick after ExpiresAt.
	terminalRetention := s.maxAge / 2
	if terminalRetention < time.Minute {
		terminalRetention = time.Minute
	}
	for sid, sess := range s.sessions {
		expired := now.After(sess.ExpiresAt) || sess.State == StateExpired
		if !expired {
			continue
		}
		// Carve-out: never prune an in-flight session even if TTL
		// elapsed. The orchestrator will transition it to a terminal
		// state itself (ON_CHAIN / FORWARD_FAILED / ATTESTATION_TIMEOUT)
		// and the NEXT prune cycle will reclaim it. R3-CSM-02 +
		// R4-SEC-01 use SessionState.IsInFlight to share the predicate
		// with SessionStore.Get's overwrite-guard.
		if sess.State.IsInFlight() {
			continue
		}
		// Round-5 R5-CORR-01: hold non-Expired terminal states
		// (AttestationTimeout, SlowPathPending, ForwardFailed,
		// OnChain) past ExpiresAt for an additional retention
		// window so CountByState can surface the distinction. Only
		// reclaim once both ExpiresAt + terminalRetention have
		// elapsed (or the state explicitly transitioned to Expired).
		if sess.State != StateExpired && sess.State.IsTerminal() {
			if !now.After(sess.ExpiresAt.Add(terminalRetention)) {
				continue
			}
		}
		delete(s.sessions, sid)
		removed++
		if sess.DepositAddress != "" {
			prunedDepositAddrs = append(prunedDepositAddrs, sess.DepositAddress)
		}
	}
	return removed, prunedDepositAddrs
}

// Len returns the current session count.
func (s *SessionStore) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.sessions)
}

// CountByIP returns the number of non-terminal sessions for an IP.
// Used by rate limits in the HTTP layer.
func (s *SessionStore) CountByIP(ip string) int {
	// IP indexing intentionally not implemented in v1 — store is bounded
	// at low cardinality (sessions/sec is small) and a linear scan is
	// fine for the per-IP cap check. Add an index when load warrants.
	s.mu.RLock()
	defer s.mu.RUnlock()
	// TODO: when we add a CreatedByIP field; for now return 0 so the
	// rate-limit check is a no-op. Frontend rate-limiting is the primary
	// defense; this is defense in depth.
	_ = ip
	return 0
}

// GenerateSid returns a fresh 128-bit hex-encoded random nonce. The IS
// service uses this as the session id AND the sid embedded in the
// deposit-address instruction (per spec §5.6 — sid baked in makes
// addresses per-session-unique without on-chain registration).
func GenerateSid() (string, error) {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// BuildAuthInstruction / BuildCallInstruction delegate to
// vsc-node/lib/islock-instruction (round-3 audit R3-09 — the shared
// package is the single source-of-truth that the cross-repo parity
// test imports directly, replacing hand-mirrored test closures).
func BuildAuthInstruction(sid string) string {
	return islockinstruction.BuildAuthInstruction(sid)
}

func BuildCallInstruction(contractId, method, argsB64, sid string, amountDuffs int64) string {
	return islockinstruction.BuildCallInstruction(contractId, method, argsB64, sid, amountDuffs)
}

// MinDustDuffs is the floor below which the contract rejects an IS payment.
// Matches dash-mapping-contract's policy (10,000 duffs = 0.0001 DASH).
// Below this the IS-network-fee swamps the credit amount and there's no
// economic point in processing.
const MinDustDuffs int64 = 10_000

// MinCallFundingDuffs is the floor for value-bearing op=call (1,000,000
// duffs = 0.01 DASH). Spec §5.2.7.
const MinCallFundingDuffs int64 = 1_000_000

// formatInt — strconv-free int formatting to keep imports tight in main.
func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

// ErrSessionNotFound is returned by handlers when a sid is unknown.
var ErrSessionNotFound = errors.New("session not found")
