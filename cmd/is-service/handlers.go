package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"
)

// AddressSigner produces a signature over (depositAddress || instruction)
// using a pinned key. Per spec §5.7 the key MUST live in an HSM/KMS, not
// in process memory — see the AddressSignerHMAC implementation below for
// the v1 development stub, and the AddressSignerHSM interface for the
// production path.
//
// The signature is verified by the Altera frontend against a pinned
// public key. This closes the address-substitution vector that would
// otherwise allow a compromised IS-service host to swap addresses.
type AddressSigner interface {
	Sign(depositAddress, instruction string) (string, error)
}

// AddressSignerHMAC is a v1-development stub. It signs with HMAC-SHA256
// using a symmetric secret. The frontend would pin the same secret —
// this is INSECURE for production: anyone with the binary's secret can
// forge signatures. Production needs an HSM/KMS asymmetric signer where
// the frontend pins the public key and only the HSM holds the private.
//
// Use this only for dev/test runs. The Server requires an explicit flag
// to construct it.
type AddressSignerHMAC struct {
	secret []byte
}

func NewAddressSignerHMAC(secret []byte) *AddressSignerHMAC {
	return &AddressSignerHMAC{secret: append([]byte(nil), secret...)}
}

func (h *AddressSignerHMAC) Sign(depositAddress, instruction string) (string, error) {
	if len(h.secret) == 0 {
		return "", fmt.Errorf("address signer secret empty — refusing to sign")
	}
	mac := hmac.New(sha256.New, h.secret)
	mac.Write([]byte(depositAddress))
	mac.Write([]byte{0}) // delimiter
	mac.Write([]byte(instruction))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil)), nil
}

// Server holds the IS service state. One per process.
type Server struct {
	primaryPubKey string
	backupPubKey  string
	chainParams   *chaincfg.Params
	chainID       string
	sessions      *SessionStore
	sessionTTL    time.Duration
	signer        AddressSigner
	rateLimitsIP  *rateLimiter
	// dashd is optional — when nil, the IS-observed transition is
	// driven externally (e.g. for tests). When non-nil, Server.Run
	// starts the watcher goroutine.
	dashd *DashdWatcher
	// orch is optional — when nil, the IS_OBSERVED → ATTESTING → ON_CHAIN
	// progression is left for an external driver (tests, or a separate
	// process during devnet bring-up). When non-nil, onISLockObserved
	// spawns a goroutine that runs the full p2p attestation + L2 submit
	// flow.
	orch *Orchestrator
}

type ServerConfig struct {
	PrimaryPubKeyHex string
	BackupPubKeyHex  string
	Network          string // "mainnet" or "testnet"
	ChainID          string // "vsc-mainnet" or "vsc-testnet"
	SessionTTL       time.Duration
	Signer           AddressSigner
	// Dashd is optional. If non-nil, the Server registers each new
	// session's deposit address with the watcher and transitions the
	// session to IS_OBSERVED when the watcher detects an IS-locked tx
	// paying that address.
	Dashd *DashdWatcher
	// Orch is optional. If non-nil, onISLockObserved kicks off the
	// p2p-attestation + L2-submit flow in a goroutine after marking
	// the session IS_OBSERVED.
	Orch *Orchestrator
	// Sessions optionally injects a pre-built SessionStore so external
	// constructions (e.g. an orchestrator built before the server) can
	// share state. If nil, NewServer creates a fresh one.
	Sessions *SessionStore
}

// NewServer constructs a Server. Returns an error if config is invalid.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.PrimaryPubKeyHex == "" || cfg.BackupPubKeyHex == "" {
		return nil, fmt.Errorf("bridge pubkeys must be set")
	}
	if cfg.Signer == nil {
		return nil, fmt.Errorf("address signer must be configured (see §5.7 — HSM/KMS in prod)")
	}
	if cfg.ChainID == "" {
		return nil, fmt.Errorf("chainID must be set")
	}
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 30 * time.Minute
	}
	var params *chaincfg.Params
	switch cfg.Network {
	case "mainnet":
		params = dashMainNetParams()
	case "testnet":
		params = dashTestNetParams()
	default:
		return nil, fmt.Errorf("network must be 'mainnet' or 'testnet', got %q", cfg.Network)
	}
	sessions := cfg.Sessions
	if sessions == nil {
		sessions = NewSessionStore(cfg.SessionTTL)
	}
	return &Server{
		primaryPubKey: cfg.PrimaryPubKeyHex,
		backupPubKey:  cfg.BackupPubKeyHex,
		chainParams:   params,
		chainID:       cfg.ChainID,
		sessions:      sessions,
		sessionTTL:    cfg.SessionTTL,
		signer:        cfg.Signer,
		rateLimitsIP:  newRateLimiter(10, time.Minute),
		dashd:         cfg.Dashd,
		orch:          cfg.Orch,
	}, nil
}

// onISLockObserved is the dashd-watcher callback. Transitions a session
// from WAITING_FOR_IS → IS_OBSERVED and stores the dash txid + rawTxHex
// (the rawTxHex is needed later for the mapInstantSendV2 submission).
//
// Idempotent: if the session is already past WAITING_FOR_IS (e.g.
// already in ATTESTING or ON_CHAIN), the callback no-ops. This handles
// the watcher's "may fire multiple times" semantic safely.
func (s *Server) onISLockObserved(sid, txid, rawTxHex string) {
	advanced := false
	s.sessions.MutateState(sid, func(sess *Session) {
		if sess.State != StateWaitingForIS {
			return
		}
		sess.State = StateISObserved
		sess.DashTxId = txid
		advanced = true
	})
	if !advanced || s.orch == nil {
		return
	}
	// Orchestrator drives the rest of the state machine in its own
	// goroutine — broadcasting attestation requests, collecting
	// quorum, submitting the L2 tx. Context is detached from the
	// watcher's so a slow attestation doesn't block IS-observation
	// of other sessions; orchestrator has its own per-phase timeouts.
	go s.orch.Drive(context.Background(), sid, txid, rawTxHex)
}

// Routes returns an http.ServeMux with the IS service's HTTP endpoints
// wired up. Caller decides how to listen (e.g. with TLS, behind a
// reverse proxy, etc.).
func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /session/start", s.handleSessionStart)
	mux.HandleFunc("GET /session/{sid}/status", s.handleSessionStatus)
	mux.HandleFunc("POST /session/{sid}/cancel", s.handleSessionCancel)
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	return mux
}

// ===== request / response types =====

type SessionStartRequest struct {
	Op string `json:"op"` // "auth" or "call"
	// Args is only consulted for op=call. Encoded base64-tinyjson to
	// match the contract's instruction grammar.
	Args *CallArgs `json:"args,omitempty"`
	// Sid is OPTIONAL client-generated nonce. If empty, server generates.
	Sid string `json:"sid,omitempty"`
}

type CallArgs struct {
	Contract    string `json:"contract"`
	Method      string `json:"method"`
	ArgsB64     string `json:"args"`
	AmountDuffs int64  `json:"amount,omitempty"`
}

type SessionStartResponse struct {
	Sid                   string `json:"sid"`
	DepositAddress        string `json:"depositAddress"`
	DepositInstructionHex string `json:"depositInstructionHex"`
	AddressSignature      string `json:"addressSignature"`
	RequiredAmountDuffs   int64  `json:"requiredAmountDuffs"`
	ExpiresAt             string `json:"expiresAt"`
	StatusURL             string `json:"statusUrl"`
}

type SessionStatusResponse struct {
	Sid          string  `json:"sid"`
	State        string  `json:"state"`
	DashTxId     string  `json:"dashTxId,omitempty"`
	ForwardedAt  string  `json:"forwardedAt,omitempty"`
	SessionToken string  `json:"sessionToken,omitempty"`
	ForwardError string  `json:"forwardError,omitempty"`
	ExpiresAt    string  `json:"expiresAt"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// ===== handlers =====

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !s.rateLimitsIP.allow(ip) {
		writeError(w, http.StatusTooManyRequests, "rate limit exceeded for source IP")
		return
	}

	var req SessionStartRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	op := Op(req.Op)
	switch op {
	case OpAuth, OpCall:
	default:
		writeError(w, http.StatusBadRequest, "op must be 'auth' or 'call'")
		return
	}

	sid := req.Sid
	if sid == "" {
		var err error
		sid, err = GenerateSid()
		if err != nil {
			writeError(w, http.StatusInternalServerError, "could not generate sid")
			return
		}
	}
	if existing, ok := s.sessions.Get(sid); ok && !existing.State.IsTerminal() {
		writeError(w, http.StatusConflict, "session with this sid already active")
		return
	}

	var instruction string
	var requiredAmount int64
	switch op {
	case OpAuth:
		instruction = BuildAuthInstruction(sid)
		requiredAmount = MinDustDuffs // 0.0001 DASH

	case OpCall:
		if req.Args == nil || req.Args.Contract == "" || req.Args.Method == "" {
			writeError(w, http.StatusBadRequest, "op=call requires args.contract and args.method")
			return
		}
		if req.Args.AmountDuffs > 0 && req.Args.AmountDuffs < MinCallFundingDuffs {
			writeError(w, http.StatusBadRequest,
				fmt.Sprintf("op=call with amount>0 must be at least %d duffs (0.01 DASH); got %d",
					MinCallFundingDuffs, req.Args.AmountDuffs))
			return
		}
		instruction = BuildCallInstruction(req.Args.Contract, req.Args.Method, req.Args.ArgsB64, sid, req.Args.AmountDuffs)
		if req.Args.AmountDuffs > 0 {
			requiredAmount = req.Args.AmountDuffs
		} else {
			requiredAmount = MinDustDuffs
		}
	}

	depositAddr, _, err := DepositAddress(s.primaryPubKey, s.backupPubKey, instruction, s.chainParams)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "deposit address derivation failed: "+err.Error())
		return
	}

	sig, err := s.signer.Sign(depositAddr, instruction)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "address signing failed: "+err.Error())
		return
	}

	now := time.Now()
	sess := &Session{
		Sid:              sid,
		Op:               op,
		Instruction:      instruction,
		DepositAddress:   depositAddr,
		RequiredAmount:   requiredAmount,
		AddressSignature: sig,
		CreatedAt:        now,
		ExpiresAt:        now.Add(s.sessionTTL),
		State:            StateWaitingForIS,
	}
	s.sessions.Put(sess)

	// Register the deposit address with the dashd watcher so we get
	// notified when an IS-lock fires. Watcher is optional — if not
	// configured (e.g. in tests), state transitions are driven
	// externally.
	if s.dashd != nil {
		s.dashd.Watch(depositAddr, sid, s.onISLockObserved)
	}

	writeJSON(w, http.StatusCreated, SessionStartResponse{
		Sid:                   sid,
		DepositAddress:        depositAddr,
		DepositInstructionHex: hexBytes(instruction),
		AddressSignature:      sig,
		RequiredAmountDuffs:   requiredAmount,
		ExpiresAt:             sess.ExpiresAt.Format(time.RFC3339),
		StatusURL:             "/session/" + sid + "/status",
	})
}

func (s *Server) handleSessionStatus(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if sid == "" {
		writeError(w, http.StatusBadRequest, "missing sid in path")
		return
	}
	sess, ok := s.sessions.Get(sid)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	resp := SessionStatusResponse{
		Sid:          sess.Sid,
		State:        string(sess.State),
		DashTxId:     sess.DashTxId,
		SessionToken: sess.SessionToken,
		ForwardError: sess.ForwardError,
		ExpiresAt:    sess.ExpiresAt.Format(time.RFC3339),
	}
	if sess.OnChainAt != nil {
		resp.ForwardedAt = sess.OnChainAt.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleSessionCancel(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("sid")
	if sid == "" {
		writeError(w, http.StatusBadRequest, "missing sid in path")
		return
	}
	ok := s.sessions.MutateState(sid, func(sess *Session) {
		if !sess.State.IsTerminal() {
			sess.State = StateExpired
		}
	})
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ===== utility =====

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, ErrorResponse{Error: msg})
}

func clientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// First entry is the original client per common convention.
		if i := strings.Index(fwd, ","); i >= 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return strings.TrimSpace(fwd)
	}
	host, _, _ := splitHostPort(r.RemoteAddr)
	return host
}

func splitHostPort(addr string) (string, string, error) {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr, "", nil
	}
	return addr[:i], addr[i+1:], nil
}

// hexBytes returns hex(s). Used to encode the instruction string for the
// frontend's `depositInstructionHex` field — gives the user a tamper-
// detectable representation without needing to deal with shell-escaping.
func hexBytes(s string) string {
	const hexdigits = "0123456789abcdef"
	out := make([]byte, len(s)*2)
	for i, b := range []byte(s) {
		out[i*2] = hexdigits[b>>4]
		out[i*2+1] = hexdigits[b&0xF]
	}
	return string(out)
}

// ===== rate limiter =====

// rateLimiter is a per-IP token-bucket. Bounded to maxKeys to avoid memory
// growth from an attacker generating fresh IPs.
type rateLimiter struct {
	maxPerWindow int
	window       time.Duration
	state        sync.Map // key string -> *bucketState
	now          func() time.Time
	keyCount     atomic.Int64
}

type bucketState struct {
	mu       sync.Mutex
	count    int
	windowAt time.Time
}

const rateLimiterMaxKeys = 100_000

func newRateLimiter(maxPerWindow int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		maxPerWindow: maxPerWindow,
		window:       window,
		now:          time.Now,
	}
}

func (r *rateLimiter) allow(key string) bool {
	if key == "" {
		return true // never block missing-IP requests
	}
	if r.keyCount.Load() >= rateLimiterMaxKeys {
		// Memory cap reached — fail open for new keys. Existing keys
		// continue to track normally.
		v, ok := r.state.Load(key)
		if !ok {
			return true
		}
		b := v.(*bucketState)
		return r.checkBucket(b)
	}
	bAny, loaded := r.state.LoadOrStore(key, &bucketState{windowAt: r.now()})
	if !loaded {
		r.keyCount.Add(1)
	}
	return r.checkBucket(bAny.(*bucketState))
}

func (r *rateLimiter) checkBucket(b *bucketState) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := r.now()
	if now.Sub(b.windowAt) > r.window {
		b.windowAt = now
		b.count = 0
	}
	if b.count >= r.maxPerWindow {
		return false
	}
	b.count++
	return true
}
