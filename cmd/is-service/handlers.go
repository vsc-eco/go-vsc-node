package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/btcsuite/btcd/chaincfg"

	islock "vsc-node/modules/islock-attestation"
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
	// broadcasterHealth: optional probe surfaced via /healthz.
	broadcasterHealth func() (int, bool)
	// dashdHealth: optional DashdWatcher probe surfaced via /healthz.
	// Round-3 audit OP-003.
	dashdHealth func() (consecutiveFails int, lastErr error, lastErrAt time.Time)
	// submitterHealth: optional probe surfacing the L2 submitter DID +
	// HBD balance + RC remaining + ConsecutiveFails on /healthz.
	// Round-4 audit R4-007 — without this an RC-exhaustion outage
	// looks identical to ordinary quiet traffic on /healthz. Round-6
	// audit R6-CORR-02 extended the signature with consecutiveFails
	// so the >= submitterDegradedFailThreshold hysteresis from
	// submitter_health.go is actually applied at the /healthz
	// degraded-flag gate.
	submitterHealth func() (did string, balanceHbd int64, rcRemaining int64, consecutiveFails int, err error)
	// trustedProxies: additional hosts (beyond loopback) that the
	// clientIP helper trusts for X-Forwarded-For. Empty means
	// loopback-only (M5 default). Audit TC2-06 plugged the docstring
	// gap where the config field was promised but missing.
	trustedProxies []string
	// driveCtx + driveWg track spawned Drive goroutines so graceful
	// shutdown can drain them before tearing down the broadcaster.
	// Audit `orchestrator-detached-context-on-shutdown`. Default ctx
	// is context.Background when not wired (test paths).
	driveCtx    context.Context
	driveCancel context.CancelFunc
	driveWg     sync.WaitGroup
	// processStartedAt is stamped in NewServer for /healthz; lets
	// dashboards compute rate() over orchestrator counters
	// (R5-OP-03 — counters are atomic.Int64 and reset on restart).
	processStartedAt time.Time
	// testEndpointsEnabled: see ServerConfig.TestEndpointsEnabled doc.
	testEndpointsEnabled bool
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
	// BroadcasterHealth is an optional probe the /healthz handler calls
	// to surface the libp2p connect-count + degraded flag. Wired by
	// main when a real broadcaster is configured. Returns (connectedPeers, degraded).
	BroadcasterHealth func() (int, bool)
	// DashdHealth surfaces DashdWatcher's consecutive-failure count
	// via /healthz. Round-3 audit OP-003 — without this the watcher
	// can be functionally dead while /healthz reports green.
	DashdHealth func() (consecutiveFails int, lastErr error, lastErrAt time.Time)
	// SubmitterHealth surfaces the L2 submitter funding state on
	// /healthz: derived DID, HBD balance (in cents), RC remaining,
	// and the monitor's consecutive-fails counter. Round-4 audit
	// R4-007 added the original surface; round-6 audit R6-CORR-02
	// added consecutiveFails so /healthz can apply the
	// submitterDegradedFailThreshold hysteresis.
	SubmitterHealth func() (did string, balanceHbd int64, rcRemaining int64, consecutiveFails int, err error)
	// TrustedProxies: literal host/IP strings (loopback always implied)
	// from which X-Forwarded-For is honoured. Audit TC2-06: M5 docstring
	// promised this and the field didn't exist; now it does.
	TrustedProxies []string
	// TestEndpointsEnabled wires the test-only `/test/observed/{sid}`
	// endpoint that synthesises an onISLockObserved callback from the
	// HTTP request body. **TEST-ONLY** — main.go only sets this when
	// args.testBypassDashdISLock is true. The IS service now generates
	// base58 P2SH deposit addresses (commit acfb268) so the dashd
	// watcher's address-fan-out works natively against regtest dashd
	// — the /test/observed bypass is retained for historical paths +
	// /test/attestation that don't go through the watcher.
	TestEndpointsEnabled bool
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
	case "devnet":
		// Devnet runs dashd in regtest mode for the
		// tests/devnet IS-login E2E suite. Address encoding
		// inherits testnet params — the test driver doesn't
		// validate against a strict regtest prefix set
		// because tests/devnet's dashd RPC works fine with
		// the testnet prefixes. Audit R15-CONS-11: args.go
		// accepts -network=devnet without refusing in production;
		// the only devnet-gated flag is -testBypassDashdISLock.
		// Operational expectation: production deploys MUST use
		// -network=mainnet or -network=testnet (and the
		// fail-fast on unknown values still backstops typos).
		params = dashTestNetParams()
	default:
		return nil, fmt.Errorf("network must be 'mainnet', 'testnet' or 'devnet', got %q", cfg.Network)
	}
	sessions := cfg.Sessions
	if sessions == nil {
		sessions = NewSessionStore(cfg.SessionTTL)
	}
	driveCtx, driveCancel := context.WithCancel(context.Background())
	return &Server{
		primaryPubKey:     cfg.PrimaryPubKeyHex,
		backupPubKey:      cfg.BackupPubKeyHex,
		chainParams:       params,
		chainID:           cfg.ChainID,
		sessions:          sessions,
		sessionTTL:        cfg.SessionTTL,
		signer:            cfg.Signer,
		rateLimitsIP:      newRateLimiter(10, time.Minute),
		dashd:             cfg.Dashd,
		orch:              cfg.Orch,
		broadcasterHealth: cfg.BroadcasterHealth,
		dashdHealth:       cfg.DashdHealth,
		submitterHealth:   cfg.SubmitterHealth,
		trustedProxies:       append([]string(nil), cfg.TrustedProxies...),
		testEndpointsEnabled: cfg.TestEndpointsEnabled,
		driveCtx:             driveCtx,
		driveCancel:          driveCancel,
		processStartedAt:  time.Now(),
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
	// Best-effort sender resolution. Failure isn't fatal — the contract
	// re-derives this from rawTxHex on its own. We only surface the
	// address through /status as a UX convenience.
	sender, senderErr := resolveSenderAddress(rawTxHex, s.chainParams)
	advanced := false
	var depositAddr string
	s.sessions.MutateState(sid, func(sess *Session) {
		if sess.State != StateWaitingForIS {
			return
		}
		sess.State = StateISObserved
		sess.DashTxId = txid
		depositAddr = sess.DepositAddress
		if senderErr == nil {
			sess.SenderAddress = sender
		}
		advanced = true
	})
	if !advanced {
		return
	}
	// IS-lock observed — we no longer need to watch this address.
	// Unwatch before kicking off orch.Drive so the watcher map shrinks
	// promptly under load (audit `dashd-watcher-leaks-after-session-terminal`).
	if s.dashd != nil && depositAddr != "" {
		s.dashd.Unwatch(depositAddr)
	}
	if s.orch == nil {
		return
	}
	// Orchestrator drives the rest of the state machine in its own
	// goroutine — broadcasting attestation requests, collecting
	// quorum, submitting the L2 tx. Tracked via WaitGroup so graceful
	// shutdown can drain in-flight work before closing the broadcaster
	// (audit `orchestrator-detached-context-on-shutdown`).
	s.driveWg.Add(1)
	go func() {
		defer s.driveWg.Done()
		s.orch.Drive(s.driveCtx, sid, txid, rawTxHex)
	}()
}

// Drain blocks until all in-flight Drive goroutines finish, or until
// the deadline elapses. Call AFTER httpSrv.Shutdown and BEFORE
// p2pBroadcaster.Close so in-flight L2 submissions can finish their
// GraphQL roundtrip and so attestation broadcasts don't fail with
// publish-on-closed-topic. Audit `orchestrator-detached-context-on-shutdown`.
func (s *Server) Drain(deadline time.Duration) {
	// Round-5 audit R5-CORRECT-02: defer ensures driveCancel runs on
	// every Drain return, including the clean-drain branch (the bug
	// that round-5 fixed — cancel was leaked on the happy path).
	//
	// Round-6 audit R6-CORR-01: the hard-cap branch ADDITIONALLY
	// needs an explicit driveCancel BEFORE the `<-done` wait. Drive
	// goroutines block on context-aware GraphQL / reconcileL2 work;
	// they only return promptly once driveCtx is cancelled. With
	// only the deferred cancel, `<-done` would wait for the full
	// natural ~225s budget instead of cutting Drives short at the
	// deadline. context.CancelFunc is idempotent — calling it twice
	// is safe and explicit.
	defer s.driveCancel()
	done := make(chan struct{})
	go func() {
		s.driveWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(deadline):
		// Hard cap reached — cancel the shared driveCtx so any
		// in-flight GraphQL POST / reconcile sleep aborts promptly.
		s.driveCancel()
		<-done
	}
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
	// Test-only endpoints — registered only when TestEndpointsEnabled
	// is true (which main.go gates on -testBypassDashdISLock).
	// Originally added because Dash never activated SegWit, so an
	// earlier bech32 P2WSH deposit-address implementation was
	// unpayable on regtest dashd; tests had to inject observation
	// state directly. The IS service now generates P2SH addresses
	// (commit acfb268) so the watcher works against regtest natively
	// — the /test/observed + /test/attestation endpoints are kept
	// for tests that synthesise specific orchestrator states without
	// involving the watcher or gossip path.
	if s.testEndpointsEnabled {
		mux.HandleFunc("POST /test/observed/{sid}", s.handleTestObserved)
		mux.HandleFunc("POST /test/attestation/{sid}", s.handleTestAttestation)
	}
	return mux
}

// TestObservedRequest carries the synthesised observation payload
// for the test-only /test/observed/{sid} endpoint.
type TestObservedRequest struct {
	TxId     string `json:"txid"`
	RawTxHex string `json:"rawTxHex,omitempty"`
}

// handleTestObserved synthesises an onISLockObserved callback. The
// orchestrator's downstream behaviour is unchanged — broadcast
// attestation requests, collect, submit L2. Returns 200 on success
// (the orchestrator goroutine spawns asynchronously; the caller
// should poll /session/{sid}/status to observe the transition).
func (s *Server) handleTestObserved(w http.ResponseWriter, r *http.Request) {
	if !s.testEndpointsEnabled {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	sid := r.PathValue("sid")
	if sid == "" {
		writeError(w, http.StatusBadRequest, "missing sid")
		return
	}
	var req TestObservedRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err.Error() != "EOF" {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if req.TxId == "" {
		req.TxId = "test-observed-" + sid
	}
	// onISLockObserved no-ops if the session isn't in
	// StateWaitingForIS, so calling against a missing/terminal
	// session is safe. Use the configured handler so all the same
	// side effects (Unwatch, orchestrator Drive goroutine, etc.) fire.
	s.onISLockObserved(sid, req.TxId, req.RawTxHex)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// handleTestAttestation forwards a JSON-supplied
// IsLockAttestationResponse straight into the orchestrator's
// collector. Bypasses the libp2p gossip — used by tests/devnet so
// the test driver can synthesise attestations without wiring real
// validators into the magi-node binary. Returns 200 on accept.
func (s *Server) handleTestAttestation(w http.ResponseWriter, r *http.Request) {
	if !s.testEndpointsEnabled {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	if s.orch == nil {
		writeError(w, http.StatusServiceUnavailable, "orchestrator not configured")
		return
	}
	var resp islock.IsLockAttestationResponse
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	if resp.TxId == "" || resp.ValidatorDID == "" || resp.PubkeyHex == "" || resp.BlsSigHex == "" {
		writeError(w, http.StatusBadRequest,
			"required fields: txid, validatorDid, pubkey, sig")
		return
	}
	s.orch.DeliverAttestation(resp)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
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
	Sid           string `json:"sid"`
	State         string `json:"state"`
	DashTxId      string `json:"dashTxId,omitempty"`
	SenderAddress string `json:"senderAddress,omitempty"`
	ForwardedAt   string `json:"forwardedAt,omitempty"`
	SessionToken  string `json:"sessionToken,omitempty"`
	// L2TxId is the mapInstantSendV2 transaction CID. Populated once
	// the orchestrator hands the L2 tx to the GraphQL endpoint
	// (StateL2Submitted) AND preserved on the divergent terminal
	// states (StateExpired, StateForwardFailed) so operators and
	// frontends can recover an L2-credited-but-session-lost payment
	// from /status alone — without log scraping. Round-4 audit R4-006.
	L2TxId       string `json:"l2TxId,omitempty"`
	ForwardError string `json:"forwardError,omitempty"`
	ExpiresAt    string `json:"expiresAt"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

// ===== handlers =====

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{
		"ok":       true,
		"sessions": s.sessions.Len(),
		// Round-5 audit R5-OP-03: orchestrator counters reset on every
		// restart, so absolute-value alert thresholds would page
		// spuriously on rolling deploys. processStartedAt lets
		// dashboards compute rate() correctly and lets operators see
		// at a glance whether the counters they're looking at are
		// from this process or a previous incarnation.
		"processStartedAt": s.processStartedAt.UTC().Format(time.RFC3339),
		// Round-6 audit R6-OP-03: pair the RFC3339 string with a
		// Unix-seconds integer for Prometheus/Grafana scrapers that
		// expect process_start_time_seconds-style numeric values.
		// rate() math over the atomic counters can use either field.
		"processStartedAtUnix": s.processStartedAt.Unix(),
	}
	// Round-3 audit OP-004: expose per-state session counts so operators
	// can spot pile-ups in L2_SUBMITTED (reconciler stuck), ATTESTING
	// (validators not responding), etc.
	out["sessionsByState"] = s.sessions.CountByState()

	// Broadcaster degraded flag — set by main wiring when libp2p is
	// configured. Lets ops dashboards surface a zero-peers degraded
	// state without restart-with-debug.
	if s.broadcasterHealth != nil {
		conn, degraded := s.broadcasterHealth()
		out["connectedPeers"] = conn
		out["broadcasterDegraded"] = degraded
		if degraded {
			out["ok"] = false
		}
	}

	// Round-3 audit OP-003: surface DashdWatcher consecutive-failure
	// count. When fails >= 5 (the same threshold the watcher uses to
	// escalate to slog.Error), flip /healthz red so synthetic probes
	// page the operator.
	if s.dashdHealth != nil {
		fails, lastErr, _ := s.dashdHealth()
		out["dashdWatcherConsecutiveFails"] = fails
		degraded := fails >= 5
		out["dashdWatcherDegraded"] = degraded
		if degraded {
			out["ok"] = false
			if lastErr != nil {
				out["dashdWatcherLastErr"] = lastErr.Error()
			}
		}
	}

	// Round-4 audit R4-007: surface L2 submitter funding state. An
	// RC-exhausted submitter manifests as silent ATTESTING pile-ups
	// today; this turns it into an explicit /healthz red.
	//
	// Round-6 audit R6-CORR-02: apply the consecutiveFails hysteresis
	// promised by submitter_health.go before flipping degraded. A
	// single transient GraphQL flap would previously page on every
	// probe interval; now we only flip after
	// submitterDegradedFailThreshold consecutive failures.
	//
	// Round-6 audit R6-CORR-03: the errSubmitterWarmup sentinel
	// distinguishes "monitor goroutine hasn't completed its first
	// probe yet" from a real failure. Treat warmup as a transient
	// state — surface it but don't flip degraded.
	if s.submitterHealth != nil {
		did, balance, rc, fails, err := s.submitterHealth()
		switch {
		case errors.Is(err, errSubmitterWarmup):
			// Round-7 audit R7-OP-01-readiness + R7-CORR-01-gvn:
			// during the monitor's first-probe warmup window
			// (typically <5s after process start) we flip ok=false
			// so /healthz returns 503 and synthetic readiness
			// probes don't mark the pod live before the submitter
			// probe proves L2 reachability. We also OMIT
			// submitterBalanceHbdCents / submitterRcRemaining so
			// metric scrapers without a warmup-gate don't fire
			// rc<=0 alerts on the zero-init values.
			out["submitterDID"] = did
			out["submitterWarmup"] = true
			out["submitterConsecutiveFails"] = fails
			out["ok"] = false
		case err != nil:
			out["submitterDID"] = did
			out["submitterBalanceHbdCents"] = balance
			out["submitterRcRemaining"] = rc
			out["submitterConsecutiveFails"] = fails
			out["submitterProbeErr"] = err.Error()
			if fails >= submitterDegradedFailThreshold {
				out["submitterDegraded"] = true
				out["ok"] = false
			}
		default:
			out["submitterDID"] = did
			out["submitterBalanceHbdCents"] = balance
			out["submitterRcRemaining"] = rc
			out["submitterConsecutiveFails"] = fails
			if rc <= 0 && did != "" {
				out["submitterDegraded"] = true
				out["ok"] = false
			}
		}
	}

	// Round-4 audit R4-007 / R4-006: aggregate counters for events
	// that previously only surfaced as individual slog lines.
	if s.orch != nil {
		c := s.orch.Counters()
		out["counters"] = c
		// L2-credited-but-session-lost is a real-funds divergence —
		// any non-zero count should attract operator attention even
		// if the rest of the system is green.
		if c.UnresolvableOnChainCredits > 0 {
			out["unresolvableOnChainCreditsDegraded"] = true
		}
	}

	if out["ok"].(bool) {
		writeJSON(w, http.StatusOK, out)
	} else {
		writeJSON(w, http.StatusServiceUnavailable, out)
	}
}

func (s *Server) handleSessionStart(w http.ResponseWriter, r *http.Request) {
	ip := s.clientIP(r)
	if !s.rateLimitsIP.allow(ip) {
		// Audit `rate-limit-and-libp2p-drops-silent`: log so operators
		// can spot rate-limit hits without restart-with-debug.
		slog.Debug("HTTP rate-limit exceeded", "ip", ip, "path", r.URL.Path)
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
	// Round-3 audit R3-005: the existence check is done atomically by
	// PutNew below (TOCTOU between Get and Put let two concurrent same-
	// sid requests silently clobber each other). No pre-check needed.

	// Round-2 audit D2-DESIGN-08 + Round-3 audit R3-004: reject reserved
	// delimiters AND control chars in user-supplied instruction fields
	// — for BOTH OpAuth and OpCall paths (R3-004 caught the original
	// scoping inside `case OpCall:` letting OpAuth's sid bypass the
	// check). Without this an attacker can inject duplicate keys via a
	// polluted sid / args, or embed control chars for slog injection.
	//
	// Rule:
	//   - ';' (FIELD delimiter) is reserved in ALL user fields
	//   - '=' (KV delimiter) is reserved in Contract/Method/Sid only
	//     (ArgsB64 legitimately uses '=' for base64 padding; the
	//     contract parser SplitNs on the first '=' per field)
	//   - control chars < 0x20 banned everywhere
	if hasReservedRune(sid, true /*kvReserved*/) {
		writeError(w, http.StatusBadRequest,
			"sid contains reserved or control char (';', '=', or rune < 0x20)")
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
		if hasReservedRune(req.Args.Contract, true) ||
			hasReservedRune(req.Args.Method, true) ||
			hasReservedRune(req.Args.ArgsB64, false /*kvAllowed (base64 padding)*/) {
			writeError(w, http.StatusBadRequest,
				"args fields contain reserved or control char (';' in any; '=' in contract/method; control < 0x20 in all)")
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
	if err := s.sessions.PutNew(sess); err != nil {
		if err == ErrSidAlreadyExists {
			writeError(w, http.StatusConflict, "session with this sid already active")
			return
		}
		writeError(w, http.StatusServiceUnavailable, "session capacity reached, try again later")
		return
	}

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
		Sid:           sess.Sid,
		State:         string(sess.State),
		DashTxId:      sess.DashTxId,
		SenderAddress: sess.SenderAddress,
		SessionToken:  sess.SessionToken,
		L2TxId:        sess.L2TxId,
		ForwardError:  sess.ForwardError,
		ExpiresAt:     sess.ExpiresAt.Format(time.RFC3339),
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
	// L1 — session-cancel auth: bind the cancel to a sessionToken-style
	// secret returned at session-start. Without this any sid-knower
	// could EXPIRE another user's in-flight session (audit
	// `session-cancel-no-auth`). The cancel-token is the
	// addressSignature returned in the StartResponse, since it's a
	// secret only the session owner has at this point. Header name:
	// X-Cancel-Token.
	provided := r.Header.Get("X-Cancel-Token")
	if provided == "" {
		writeError(w, http.StatusUnauthorized,
			"X-Cancel-Token header required (use addressSignature from /session/start)")
		return
	}
	var depositAddr string
	var matched bool
	var conflict bool
	s.sessions.MutateState(sid, func(sess *Session) {
		// Round-9 audit R9-INFO-TIMING-01: use crypto/subtle for the
		// AddressSignature comparison so the loop doesn't short-circuit
		// on the first mismatched byte. The signature is HMAC-SHA256
		// base64 (~256 bits) so any actual timing oracle is far below
		// network-measurement resolution, but constant-time is the
		// hygienic default for any secret-equality check.
		if subtle.ConstantTimeCompare([]byte(sess.AddressSignature), []byte(provided)) != 1 {
			return
		}
		matched = true
		// Round-4 audit R4-003: refuse cancel when the orchestrator
		// has already broadcast attestations or submitted to L2.
		// Clobbering State to Expired here defeats the Prune
		// carve-out (state is no longer in-flight, so the next
		// Prune deletes it) and races reconcileL2 into
		// L2-credited-but-session-lost divergence. The
		// orchestrator's own MutateState callback will reach a
		// terminal state on its own.
		if sess.State.IsInFlight() {
			conflict = true
			return
		}
		depositAddr = sess.DepositAddress
		if !sess.State.IsTerminal() {
			sess.State = StateExpired
		}
	})
	if !matched {
		// Either no such session OR bad token. 401 for both —
		// matches /status which returns 404 vs 200 anyway, so any
		// existence oracle is already publicly observable on the
		// status endpoint. Round-10 audit R10-INFO-EXISTENCE-ORACLE-01
		// adjusted the claim to be honest about the model.
		writeError(w, http.StatusUnauthorized, "session not found or token mismatch")
		return
	}
	if conflict {
		writeError(w, http.StatusConflict,
			"session is mid-attestation or mid-L2-submit and cannot be cancelled; "+
				"poll /session/{sid}/status until terminal")
		return
	}
	if s.dashd != nil && depositAddr != "" {
		s.dashd.Unwatch(depositAddr)
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

// clientIP returns the effective source IP for rate-limiting. ONLY
// honours X-Forwarded-For when r.RemoteAddr is in the server's
// configured trusted-proxies set (loopback by default; extensible via
// ServerConfig.TrustedProxies — audit TC2-06 plugged the
// missing-field gap). Without this gate any attacker can rotate
// X-Forwarded-For headers to bypass the per-IP cap (audit
// `xff-rate-limit-bypass`).
//
// Round-3 audit OP-005: parse via net.SplitHostPort so IPv6 brackets
// strip correctly. The previous hand-rolled splitHostPort returned
// "[::1]" with brackets, breaking the trusted-proxy comparison
// against the literal "::1" default.
func (s *Server) clientIP(r *http.Request) string {
	hostPart, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		// RemoteAddr might just be "host" with no port; use it directly.
		hostPart = r.RemoteAddr
	}
	if s.isTrustedProxy(hostPart) {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			if i := strings.Index(fwd, ","); i >= 0 {
				return strings.TrimSpace(fwd[:i])
			}
			return strings.TrimSpace(fwd)
		}
	}
	return hostPart
}

// isTrustedProxy consults both the hardcoded loopback default and the
// operator-supplied list. Loopback is always trusted; additional hosts
// come from Server.trustedProxies (literal host/IP strings — CIDR
// support is a future enhancement).
//
// Round-4 audit R4-SEC-05: net.SplitHostPort returns IPv6 hosts in
// canonical lowercase, but the operator-supplied trustedProxies list
// may carry mixed/upper-case entries. Normalize both sides through
// net.ParseIP + .String() so an operator typo doesn't silently degrade
// the per-IP rate limit to a single-bucket-per-proxy fail-secure mode.
func (s *Server) isTrustedProxy(host string) bool {
	hostNorm := canonicalHostMatch(host)
	switch hostNorm {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	for _, p := range s.trustedProxies {
		if canonicalHostMatch(p) == hostNorm {
			return true
		}
	}
	return false
}

// canonicalHostMatch lowercases hostnames and runs IP literals through
// net.ParseIP.String() so "FE80::1" and "fe80::1" match. Non-IP hosts
// fall through to a plain lowercase comparison.
func canonicalHostMatch(host string) string {
	if ip := net.ParseIP(host); ip != nil {
		return ip.String()
	}
	return strings.ToLower(host)
}

// hasReservedRune returns true if s contains any rune reserved by the
// instruction grammar OR any control char (< 0x20). When kvReserved is
// true, '=' is also rejected (use this for Contract/Method/Sid where
// '=' has no legitimate role). For ArgsB64, kvReserved=false because
// base64 uses '=' for padding — the contract parser splits on the
// first '=' per field so trailing padding survives.
// Audit R3-004.
func hasReservedRune(s string, kvReserved bool) bool {
	for _, r := range s {
		if r < 0x20 {
			return true
		}
		if r == ';' {
			return true
		}
		if kvReserved && r == '=' {
			return true
		}
	}
	return false
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
		// Missing-IP requests — refuse rather than fail open. A real
		// client with a parseable RemoteAddr never hits this path; the
		// branch only fires for malformed proxies / tests, both of
		// which should be slow-pathed.
		return false
	}
	if r.keyCount.Load() >= rateLimiterMaxKeys {
		// Memory cap reached — fail CLOSED for new keys (audit
		// `dashd-watcher-leaks-after-session-terminal` flagged the
		// previous fail-open as a memory-exhaustion vector when
		// combined with the watcher leak). Existing keys continue to
		// track normally so legitimate traffic isn't lost.
		v, ok := r.state.Load(key)
		if !ok {
			return false
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
