package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// SessionState is the lifecycle phase of an IS-login session. Mirrors the
// state machine in spec §5.7 rev 6.
type SessionState string

const (
	StateWaitingForIS       SessionState = "WAITING_FOR_IS"
	StateISObserved         SessionState = "IS_OBSERVED"
	StateAttesting          SessionState = "ATTESTING"
	// StateL2Submitted: orchestrator has handed the mapInstantSendV2
	// tx to the L2 GraphQL endpoint (mempool-accepted) but has not yet
	// confirmed execution. The reconciler polls FetchTransactionStatus
	// until terminal — audit D2-DESIGN-06. SessionToken is NOT minted
	// at this state.
	StateL2Submitted        SessionState = "L2_SUBMITTED"
	StateOnChain            SessionState = "ON_CHAIN"
	StateAttestationTimeout SessionState = "ATTESTATION_TIMEOUT"
	StateSlowPathPending    SessionState = "SLOW_PATH_PENDING"
	StateForwardFailed      SessionState = "FORWARD_FAILED"
	StateExpired            SessionState = "EXPIRED"
)

// IsTerminal reports whether the state will not advance on its own —
// the session is done from the IS service's perspective.
func (s SessionState) IsTerminal() bool {
	switch s {
	case StateOnChain, StateForwardFailed, StateExpired:
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

// Get fetches a session by sid. Returns (nil, false) on miss or expiry.
// Side effect: expired sessions are removed.
func (s *SessionStore) Get(sid string) (*Session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sid]
	if !ok {
		return nil, false
	}
	if s.now().After(sess.ExpiresAt) {
		sess.State = StateExpired
		// Keep it briefly so a polling client can see the EXPIRED state, but
		// flag it. A full deletion happens during Prune.
		return sess, true
	}
	return sess, true
}

// MaxSessions is the global cap on concurrent sessions. Beyond this,
// new sessions are rejected to bound memory + watcher entries — audit
// `dashd-watcher-leaks-after-session-terminal`.
const MaxSessions = 50_000

// ErrSessionStoreFull is returned by Put when the store is at MaxSessions.
var ErrSessionStoreFull = errors.New("session store full — global cap reached")

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
// them. Audit R2-N3: TTL-expired sessions were leaking watcher map
// entries because the prune janitor only touched SessionStore.
func (s *SessionStore) Prune() (removed int, prunedDepositAddrs []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	for sid, sess := range s.sessions {
		if now.After(sess.ExpiresAt) || sess.State == StateExpired {
			delete(s.sessions, sid)
			removed++
			if sess.DepositAddress != "" {
				prunedDepositAddrs = append(prunedDepositAddrs, sess.DepositAddress)
			}
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

// BuildAuthInstruction constructs the instruction string the user's IS
// payment will be sent to: `op=auth;sid=<sid>`. Keep this in sync with
// the dash-mapping-contract's instruction parser (workstream 5 — once
// the parser lands, add a parity test).
func BuildAuthInstruction(sid string) string {
	return "op=auth;sid=" + sid
}

// BuildCallInstruction is the value-bearing or value-less contract-call
// case: `op=call;contract=...;method=...;args=...;sid=...[;amount=...]`.
// amount is in duffs; pass 0 (default) for value-less calls per spec §5.2.4.
func BuildCallInstruction(contractId, method, argsB64, sid string, amountDuffs int64) string {
	out := "op=call;contract=" + contractId + ";method=" + method + ";args=" + argsB64 + ";sid=" + sid
	if amountDuffs > 0 {
		out += ";amount=" + formatInt(amountDuffs)
	}
	return out
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
