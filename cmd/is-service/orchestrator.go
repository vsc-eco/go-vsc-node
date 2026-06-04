package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sync/atomic"
	"time"

	islock "vsc-node/modules/islock-attestation"
)

// sanitizeURLForLog strips userinfo + query string + path from a URL
// so an operator who accidentally configured -l2GqlURL with embedded
// credentials (e.g. https://user:pass@host/...) doesn't leak them
// through structured logs to a log-shipping pipeline. Round-7 audit
// R7-OP-01-logleak; round-8 audit R8-SEC-01 + R8-DESIGN-DOC-01
// closed the schemeless-input bypass and aligned the function with
// its docstring.
//
// Behavior:
//   - empty input → empty output
//   - url.Parse failure → "<redacted: unparseable URL>" (never raw)
//   - missing scheme OR host (e.g. host:port literal,
//     `user:pass@host` opaque form) → "<redacted: missing scheme>"
//   - well-formed URL → "<scheme>://<host>" (host may include port,
//     IPv6 brackets are preserved; userinfo + query + path dropped)
//
// (round-14 cleanup) sanitizeURLForLog bare variant deleted —
// R11-INFO-DEPRECATED-SANITIZER-STILL-EXPORTED. All production
// callers use sanitizeURLForLogWithFlag; tests now also call that
// directly with an empty flag string so the empty-flag default
// behaviour stays pinned without an exported alias.

// sanitizeURLForLogWithFlag is the flag-aware sanitiser variant.
// Round-9 audit R9-INFO-MARKER-HINT-01 — when a redaction marker
// fires, including the flag name in the marker saves operators a
// triage step (no need to map slog key → flag manually).
func sanitizeURLForLogWithFlag(flag, raw string) string {
	mark := func(reason string) string {
		if flag == "" {
			return "<redacted: " + reason + ">"
		}
		return "<redacted: " + flag + " " + reason + ">"
	}
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return mark("unparseable URL")
	}
	if u.Scheme == "" || u.Host == "" {
		return mark("missing scheme")
	}
	return u.Scheme + "://" + u.Host
}

// Orchestrator drives a session through the IS_OBSERVED → ATTESTING →
// ON_CHAIN sequence. One Orchestrator runs in the process; per-session
// work is spawned in goroutines.
//
// Flow per session (triggered from Server.onISLockObserved):
//
//  1. Build IsLockAttestationRequest from session + the IS-lock context.
//  2. Mark session ATTESTING.
//  3. broadcaster.BroadcastRequest publishes to the islock-attestation
//     gossip topic.
//  4. collector.Collect blocks for N-of-M responses (or timeout).
//  5. If quorum reached, submitter.SubmitMapInstantSend posts the L2 tx
//     carrying the bundle; on success mark ON_CHAIN; else FORWARD_FAILED.
//  6. If no quorum before timeout, mark ATTESTATION_TIMEOUT.
type Orchestrator struct {
	collector       *attestationCollector
	broadcaster     Broadcaster
	submitter       Submitter
	sessions        *SessionStore
	chainID         string
	quorumThreshold int
	collectTimeout  time.Duration
	submitTimeout   time.Duration
	// reconcileBackoffs is the sleep schedule between L2 status polls
	// AFTER the first immediate poll. Empty → production default
	// (4s/8s/16s/32s/60s, ~2 min total). Tests supply short values.
	// Audit D2-DESIGN-06.
	reconcileBackoffs []time.Duration
	// epochFor returns the validator-set epoch to ask attesters to sign
	// against. v1 stub returns 0; production wires in the active epoch
	// from the witness module.
	epochFor func(ctx context.Context) uint64
	// validatorSetForEpoch returns the expected {ValidatorDID →
	// PubkeyHex} map for the given epoch. When nil (or returning nil),
	// the per-sig verifier falls back to raw signature check only.
	// Audit R3-07.
	validatorSetForEpoch func(ctx context.Context, epoch uint64) map[string]string
	// validatorSetSource is a human-readable identifier for the
	// upstream that supplied validatorSetForEpoch (typically the L2
	// GraphQL endpoint URL). Surfaced on the R5-ADV-01 roster-
	// divergence slog.Error so operators don't have to ssh+ps to
	// find the URL. Round-6 audit R6-OP-01.
	validatorSetSource string
	// counters is an atomic snapshot of operational events surfaced
	// via /healthz. Round-4 audit R4-007 — the existing
	// per-event slog lines are not aggregable.
	counters orchestratorCounters
}

// orchestratorCounters aggregates per-event tallies for /healthz.
// All fields are accessed via atomic.Int64 ops; reads via Counters()
// snapshot the values without locking.
type orchestratorCounters struct {
	UnresolvableOnChainCredits atomic.Int64 // R3-CSM-01/02 guard-skipped post-submit
	AttestationVerifyDrops     atomic.Int64 // R3-07 per-sig verify failures
	ReconcileTimeouts          atomic.Int64 // R3-04 reconcileL2 budget exhausted
	PreSubmitRefusals          atomic.Int64 // R4-CSM-07 pre-submit state check
	RandReadFailures           atomic.Int64 // R16-OPS-crypto-rand-fail-orphan-session
}

// CountersSnapshot is the read-only view of orchestrator counters
// exposed to /healthz.
type CountersSnapshot struct {
	UnresolvableOnChainCredits int64 `json:"unresolvableOnChainCredits"`
	AttestationVerifyDrops     int64 `json:"attestationVerifyDrops"`
	ReconcileTimeouts          int64 `json:"reconcileTimeouts"`
	PreSubmitRefusals          int64 `json:"preSubmitRefusals"`
	RandReadFailures           int64 `json:"randReadFailures"`
}

// DeliverAttestation forwards an attestation response into the
// orchestrator's collector — equivalent to what the real broadcaster
// does when it receives a sig from a magi validator on the
// islock-attestation gossip topic. Used by the test-only
// `/test/attestation/{sid}` HTTP endpoint to inject synthesised
// attestations during tests/devnet's IS-login E2E. **TEST-ONLY**:
// the handler that calls this is gated to -network=devnet via the
// args.go testBypassDashdISLock check, which mirrors into
// TestEndpointsEnabled. No production caller exists.
func (o *Orchestrator) DeliverAttestation(resp islock.IsLockAttestationResponse) {
	o.collector.Deliver(resp)
}

// Counters returns an atomic snapshot of the orchestrator's counters.
func (o *Orchestrator) Counters() CountersSnapshot {
	return CountersSnapshot{
		UnresolvableOnChainCredits: o.counters.UnresolvableOnChainCredits.Load(),
		AttestationVerifyDrops:     o.counters.AttestationVerifyDrops.Load(),
		ReconcileTimeouts:          o.counters.ReconcileTimeouts.Load(),
		PreSubmitRefusals:          o.counters.PreSubmitRefusals.Load(),
		RandReadFailures:           o.counters.RandReadFailures.Load(),
	}
}

// Broadcaster is the p2p publish primitive. Implemented by
// islock_attestation.Service after Start; tests pass a fake.
type Broadcaster interface {
	BroadcastRequest(ctx context.Context, req islock.IsLockAttestationRequest) error
}

// OrchestratorConfig is the construction-site bag of dependencies.
type OrchestratorConfig struct {
	Sessions    *SessionStore
	Collector   *attestationCollector
	Broadcaster Broadcaster
	Submitter   Submitter
	ChainID     string
	// QuorumThreshold is N in N-of-M attestation. 0 selects a v1 default
	// of 1 (any single attester) so the flow is testable end-to-end
	// before the validator-set-at-epoch lookup lands.
	QuorumThreshold int
	// CollectTimeout bounds how long Collect waits per session.
	CollectTimeout time.Duration
	// SubmitTimeout bounds the L2 tx submission.
	SubmitTimeout time.Duration
	// ReconcileBackoffs is the L2-status-poll backoff schedule. Empty
	// → production default. Audit D2-DESIGN-06.
	ReconcileBackoffs []time.Duration
	// EpochFor is the epoch source. nil falls back to a constant 0
	// (v1 stub — see workstream 5b).
	EpochFor func(ctx context.Context) uint64
	// ValidatorSetForEpoch returns the expected {ValidatorDID → PubkeyHex}
	// for the given epoch. Used by the per-sig verifier (R3-07) to drop
	// responses whose claimed DID doesn't match the registered pubkey,
	// avoiding wasted RC on contract-side rejections. nil = no
	// pre-check (fall back to raw-sig verify only).
	ValidatorSetForEpoch func(ctx context.Context, epoch uint64) map[string]string
	// ValidatorSetSource is a human-readable identifier (typically a
	// URL) for whatever supplied ValidatorSetForEpoch. Emitted in the
	// roster-divergence slog.Error so operators know which upstream
	// to investigate. Round-6 audit R6-OP-01.
	ValidatorSetSource string
}

func NewOrchestrator(cfg OrchestratorConfig) *Orchestrator {
	if cfg.QuorumThreshold <= 0 {
		cfg.QuorumThreshold = 1
	}
	if cfg.CollectTimeout == 0 {
		cfg.CollectTimeout = 15 * time.Second
	}
	if cfg.SubmitTimeout == 0 {
		cfg.SubmitTimeout = 30 * time.Second
	}
	if cfg.EpochFor == nil {
		cfg.EpochFor = func(context.Context) uint64 { return 0 }
	}
	return &Orchestrator{
		collector:            cfg.Collector,
		broadcaster:          cfg.Broadcaster,
		submitter:            cfg.Submitter,
		sessions:             cfg.Sessions,
		chainID:              cfg.ChainID,
		quorumThreshold:      cfg.QuorumThreshold,
		collectTimeout:       cfg.CollectTimeout,
		submitTimeout:        cfg.SubmitTimeout,
		reconcileBackoffs:    cfg.ReconcileBackoffs,
		epochFor:             cfg.EpochFor,
		validatorSetForEpoch: cfg.ValidatorSetForEpoch,
		validatorSetSource:   cfg.ValidatorSetSource,
	}
}

// Drive runs the full ATTESTING → ON_CHAIN flow for a single session in
// the calling goroutine. Typically called as `go orch.Drive(...)` from
// the dashd-watcher callback path.
//
// Mutations to the session are atomic via SessionStore.MutateState; the
// orchestrator never reads session state outside of that lock except to
// derive the request fields up-front (which are immutable post-Put).
func (o *Orchestrator) Drive(ctx context.Context, sid, txid, rawTxHex string) {
	sess, ok := o.sessions.Get(sid)
	if !ok {
		slog.Warn("orchestrator: session not found", "sid", sid)
		return
	}
	if sess.State != StateISObserved {
		// Either already past, or never made it to IS_OBSERVED. Idempotent
		// no-op — onISLockObserved already transitioned us only on the
		// expected predecessor.
		slog.Debug("orchestrator: session not in IS_OBSERVED",
			"sid", sid, "state", string(sess.State))
		return
	}

	rawTxBytes, err := hex.DecodeString(rawTxHex)
	if err != nil {
		o.fail(sid, fmt.Sprintf("invalid rawTxHex: %v", err))
		return
	}
	// sha256d (double SHA-256) is Bitcoin's tx-hashing convention; the
	// dash-mapping-contract's CanonicalAttestationMessage uses sha256d
	// on the same rawTxBytes. The signing-message builder
	// (CanonicalSigningMessage) reverses display-hex → internal-bytes,
	// so we put the DISPLAY-form hex on the wire here. See audit
	// `canonical-message-txid-byte-order-drift`.
	first := sha256.Sum256(rawTxBytes)
	internal := sha256.Sum256(first[:])
	displayHash := islock.ReverseBytesCopy(internal[:])

	instrHash := sha256.Sum256([]byte(sess.Instruction))

	req := islock.IsLockAttestationRequest{
		TxId:               txid,
		RawTxHashHex:       hex.EncodeToString(displayHash),
		InstructionHashHex: hex.EncodeToString(instrHash[:]),
		Epoch:              o.epochFor(ctx),
		ChainId:            o.chainID,
	}

	// State: IS_OBSERVED → ATTESTING
	o.sessions.MutateState(sid, func(s *Session) {
		if s.State == StateISObserved {
			s.State = StateAttesting
			s.DashTxId = txid
		}
	})

	// Pre-register the awaiter BEFORE broadcast so responses arriving
	// in the broadcast→collect gap aren't silently dropped. Without
	// this, a fast validator (or test goroutine) racing the orchestrator
	// can land its Deliver before Collect runs Await, and the response
	// hits the no-awaiter early-return.
	//
	// Round-2 audit R2-002: pair the pre-Await with a defer Cancel.
	// Without it, the awaiter+channel leaks on every successful session
	// because Collect's internal Await+defer-Cancel pair only drops the
	// refcount back to 1 (the pre-Await's count). No janitor sweeps.
	collectCtx, cancel := context.WithTimeout(ctx, o.collectTimeout)
	defer cancel()
	_ = o.collector.Await(txid, o.quorumThreshold*2)
	defer o.collector.Cancel(txid)

	if err := o.broadcaster.BroadcastRequest(ctx, req); err != nil {
		o.fail(sid, fmt.Sprintf("broadcast failed: %v", err))
		return
	}

	// Sidecar goroutine that re-broadcasts the attestation request
	// every 6s within the collect window. Defends against the race
	// where a validator's dashd-RPC poller hadn't yet observed the
	// tx when the first publish landed — without a retry the
	// validator silently drops + the request is lost for the whole
	// window. Production gossipsub mesh also has propagation jitter
	// under churn; a second publish lets the IHAVE/IWANT lazy-push
	// fill any missed peers.
	//
	// Interval tuning (audit R16-OPS-rebroadcast-validator-rate-
	// limit-exhaust HIGH): 4s would fit 3 retries in the 15s
	// collectTimeout (initial + 3 rebroadcasts = 4 total per
	// session), which at the validator-side 100 msg/min per-peer
	// cap exhausts the bucket at ~6 concurrent sessions. 6s caps
	// total per-session broadcasts at 3 (initial + 2 rebroadcasts),
	// pushing the concurrent-session ceiling to ~11. Tighter than
	// production needs (mainnet attestation latency is dominated by
	// gossip-mesh propagation + BLS sign time, both ~hundreds of
	// ms), so the 1× extra retry margin is the trade we're making
	// for ops headroom.
	//
	// Stops as soon as collectCtx is cancelled (quorum reached OR
	// collect timeout fires). Audit R16-OPS-rebroadcast-error-
	// includes-context-cancel-noise (LOW): suppress
	// context.Canceled errors from the warn log so the success path
	// (quorum reached just before a tick fires) doesn't emit benign
	// noise.
	rebroadcastDone := make(chan struct{})
	go func() {
		defer close(rebroadcastDone)
		t := time.NewTicker(6 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-collectCtx.Done():
				return
			case <-t.C:
				if err := o.broadcaster.BroadcastRequest(collectCtx, req); err != nil {
					if errors.Is(err, context.Canceled) {
						// Benign: collectCtx was cancelled mid-publish
						// because quorum reached just now or the
						// timeout fired. The select on collectCtx.Done
						// above will pick it up next iteration.
						continue
					}
					// Don't fail the session for a rebroadcast error —
					// the original publish + future ticks may still get
					// quorum. Log + continue.
					slog.Warn("attestation request rebroadcast failed",
						"sid", sid, "err", err)
				}
			}
		}
	}()

	responses := o.collector.Collect(collectCtx, txid, o.quorumThreshold, o.collectTimeout)
	cancel() // explicitly trigger the rebroadcast goroutine to exit
	<-rebroadcastDone

	// Per-sig BLS verify BEFORE aggregation (round-2 audit R2-N5).
	// Without this gate, one junk-sig response from a misbehaving peer
	// is enough to satisfy QuorumThreshold=1, the orchestrator aggregates
	// junk, submits the L2 tx, and the contract rejects after RC is
	// already spent. Verifying each sig against its claimed PubkeyHex
	// is one pairing per response; cheap compared to the L2 round-trip.
	//
	// Round-3 audit R3-07: ALSO consult the expected-DID→PubkeyHex map
	// so a peer that produces a real sig under its own BLS key but
	// claims another validator's DID is dropped here, not at the
	// contract (which would still cost RC). The expected-set is best-
	// effort — when nil (e.g. tests, devnet bring-up), we fall back to
	// raw sig verify.
	var expected map[string]string
	if o.validatorSetForEpoch != nil {
		expected = o.validatorSetForEpoch(ctx, req.Epoch)
	}
	verifiedResponses := make([]islock.IsLockAttestationResponse, 0, len(responses))
	// Round-5 audit R5-ADV-01: track whether the expected set knew
	// ANY of the responders. If we have an expected set + got
	// responses but every single one was dropped as unknown, the
	// likely cause is a malicious/wrong GraphQL endpoint returning a
	// fabricated validator set. Surface that case structurally rather
	// than letting it look like a normal quorum miss.
	hadExpected := expected != nil && len(responses) > 0
	matchedAny := false
	for _, r := range responses {
		if expected != nil {
			expectedPk, known := expected[r.ValidatorDID]
			if !known {
				o.counters.AttestationVerifyDrops.Add(1)
				slog.Warn("attestation from unknown validator DID; dropping",
					"sid", sid, "txid", txid, "validatorDID", r.ValidatorDID)
				continue
			}
			if expectedPk != r.PubkeyHex {
				o.counters.AttestationVerifyDrops.Add(1)
				slog.Warn("attestation PubkeyHex doesn't match registered set; dropping",
					"sid", sid, "txid", txid, "validatorDID", r.ValidatorDID)
				continue
			}
			matchedAny = true
		}
		ok, err := islock.VerifyAttestation(req, r)
		if err != nil || !ok {
			o.counters.AttestationVerifyDrops.Add(1)
			slog.Warn("attestation per-sig verify failed; dropping",
				"sid", sid, "txid", txid, "validatorDID", r.ValidatorDID,
				"err", err)
			continue
		}
		verifiedResponses = append(verifiedResponses, r)
	}

	if hadExpected && !matchedAny {
		// All responders are unknown to the expected set. The L2
		// GraphQL endpoint is either out of date or actively
		// misbehaving; either way the orchestrator is signing into
		// a void. Surface this loudly so operators can compare
		// libp2p mesh roster vs the GraphQL set.
		slog.Error("validator-set / libp2p roster divergence: no responder matched expected set",
			"sid", sid, "epoch", req.Epoch,
			"responders", len(responses), "expectedSize", len(expected),
			"validatorSetSource", sanitizeURLForLogWithFlag("-l2GqlURL", o.validatorSetSource),
			"hint", "verify the L2 GraphQL endpoint at validatorSetSource returns the actual on-chain validator set for this epoch")
	}

	if len(verifiedResponses) < o.quorumThreshold {
		slog.Warn("attestation quorum not reached after per-sig verify",
			"sid", sid, "txid", txid,
			"verified", len(verifiedResponses),
			"collected", len(responses),
			"need", o.quorumThreshold)
		o.sessions.MutateState(sid, func(s *Session) {
			if s.State == StateAttesting {
				s.State = StateAttestationTimeout
				s.ForwardError = fmt.Sprintf("only %d/%d verified attestations within %s (collected %d, %d failed per-sig verify)",
					len(verifiedResponses), o.quorumThreshold, o.collectTimeout,
					len(responses), len(responses)-len(verifiedResponses))
			}
		})
		return
	}

	// Build per-attestation entries for the contract envelope. Each
	// validator's PubkeyHex came down on the wire — the contract
	// verifies it against the registered set at the request's epoch.
	atts := make([]MapInstantSendAttestation, 0, len(verifiedResponses))
	for _, r := range verifiedResponses {
		atts = append(atts, MapInstantSendAttestation{
			ValidatorDID: r.ValidatorDID,
			PubkeyHex:    r.PubkeyHex,
			BlsSigHex:    r.BlsSigHex,
		})
	}

	// Aggregate the per-validator signatures into one BLS signature
	// the contract verifies via crypto.bls_verify_aggregate.
	aggSigHex, err := islock.AggregateSignatures(verifiedResponses)
	if err != nil {
		o.fail(sid, fmt.Sprintf("BLS aggregate failed: %v", err))
		return
	}

	payload := MapInstantSendPayload{
		Body: MapInstantSendBody{
			RawTxHex:     rawTxHex,
			Instruction:  sess.Instruction,
			Epoch:        req.Epoch,
			Attestations: atts,
			ChainId:      req.ChainId,
		},
		Agg: MapInstantSendAgg{
			AggSigHex: aggSigHex,
		},
	}

	// Round-4 audit R4-CSM-07: pre-Submit state re-check. The cancel
	// handler can't clobber state once we're StateAttesting (R4-003
	// guards that), but we may have transitioned into other terminal
	// states (e.g. Drive ran on a session whose IS observation
	// raced a manual /cancel before Collect started). Bail rather
	// than burn submitter RC on a known-dead session.
	if pre, ok := o.sessions.Get(sid); !ok || pre.State != StateAttesting {
		state := SessionState("missing")
		if ok {
			state = pre.State
		}
		o.counters.PreSubmitRefusals.Add(1)
		slog.Warn("pre-submit state check refused L2 submission",
			"sid", sid, "state", state, "dashTxid", txid)
		o.sessions.MutateState(sid, func(s *Session) {
			if !s.State.IsTerminal() {
				s.State = StateForwardFailed
				s.ForwardError = "session not in ATTESTING state at submit boundary"
			}
		})
		return
	}

	submitCtx, submitCancel := context.WithTimeout(ctx, o.submitTimeout)
	defer submitCancel()
	l2TxID, err := o.submitter.SubmitMapInstantSend(submitCtx, payload)
	if err != nil {
		o.fail(sid, fmt.Sprintf("L2 submission failed: %v", err))
		return
	}

	// L2 mempool-accepted. Persist the L2 txID and transition to
	// L2_SUBMITTED — but do NOT mint the session token yet.
	// Round-2 audit D2-DESIGN-06: spec requires block-inclusion before
	// issuing the token. Reconciler polls L2 status until terminal.
	//
	// Round-3 audit R3-CSM-01/R3-002/R3-04: predecessor-state guard
	// prevents clobbering a session the user already cancelled (which
	// set StateExpired between BroadcastRequest and now). MutateState
	// returns false if the session is gone (Prune deleted it — see
	// R3-CSM-02); we log + bail without issuing the token.
	var advanced bool
	o.sessions.MutateState(sid, func(s *Session) {
		if s.State != StateAttesting {
			return
		}
		s.State = StateL2Submitted
		s.L2TxId = l2TxID
		advanced = true
	})
	if !advanced {
		o.counters.UnresolvableOnChainCredits.Add(1)
		slog.Warn("session was cancelled or pruned during submit; not advancing to L2_SUBMITTED",
			"sid", sid, "l2TxId", l2TxID, "dashTxid", txid,
			"note", "L2 tx may still confirm — inspect chain state to recover")
		return
	}

	if !o.reconcileL2(ctx, sid, txid, req.ChainId, l2TxID) {
		// reconcileL2 already set the failed state.
		return
	}

	// Audit SEC-8 (R15) + R16-OPS-crypto-rand-fail-orphan-session:
	// SessionToken is server-side-random, NOT derived from public
	// observables (pre-SEC-8 used sha256(sid || dashTxId || chainID)
	// — all three inputs are public, so an attacker who scrapes a
	// /status URL could compute the token locally).
	//
	// 32 bytes of crypto/rand → 64-hex-char token (~256 bits of
	// entropy). Mint BEFORE the MutateState closure (audit R16) so a
	// crypto/rand failure routes the session to FORWARD_FAILED with
	// a clear reason instead of stranding it in L2_SUBMITTED (the
	// orchestrator has no re-drive worker for L2_SUBMITTED sessions,
	// so a strand is effectively permanent — the user has paid Dash
	// + the contract has credit but no auth token will ever issue).
	var tokenBytes [32]byte
	if _, err := rand.Read(tokenBytes[:]); err != nil {
		o.counters.RandReadFailures.Add(1)
		slog.Error("crypto/rand failed minting SessionToken; session marked FORWARD_FAILED",
			"sid", sid, "l2TxId", l2TxID, "dashTxid", txid, "err", err,
			"note", "L2 tx confirmed + contract credit minted, but no token can be issued — investigate /dev/urandom availability on this host")
		o.fail(sid, fmt.Sprintf("crypto/rand failed: %v", err))
		return
	}
	tokenHex := hex.EncodeToString(tokenBytes[:])

	now := time.Now()
	advanced = false
	o.sessions.MutateState(sid, func(s *Session) {
		if s.State != StateL2Submitted {
			return
		}
		s.State = StateOnChain
		s.OnChainAt = &now
		s.SessionToken = tokenHex
		advanced = true
	})
	if !advanced {
		o.counters.UnresolvableOnChainCredits.Add(1)
		slog.Warn("session was cancelled or pruned during reconcile; not minting SessionToken",
			"sid", sid, "l2TxId", l2TxID, "dashTxid", txid,
			"note", "L2 tx confirmed but session is no longer eligible — inspect chain state to recover")
		return
	}

	slog.Info("session reached ON_CHAIN",
		"sid", sid, "dashTxid", txid, "l2TxId", l2TxID,
		"attestations", len(verifiedResponses))
}

// reconcileL2 polls the submitter for the L2 tx's terminal status.
// Returns true on success (CONFIRMED / PROCESSED), false on terminal
// failure or timeout. On false the session is already marked
// FORWARD_FAILED via o.fail. Audit D2-DESIGN-06.
func (o *Orchestrator) reconcileL2(ctx context.Context, sid, dashTxID, chainID, l2TxID string) bool {
	// Backoffs control the wait BEFORE each poll attempt (after the
	// first). The first poll runs immediately so devnet stubs that
	// instantly return CONFIRMED don't block the test for several
	// seconds. Total budget ~2 min for the default schedule.
	backoffs := o.reconcileBackoffs
	if len(backoffs) == 0 {
		backoffs = []time.Duration{
			4 * time.Second, 8 * time.Second,
			16 * time.Second, 32 * time.Second,
			60 * time.Second,
		}
	}
	for attempt := 0; attempt <= len(backoffs); attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				o.fail(sid, fmt.Sprintf("L2 reconcile aborted: %v (l2TxId=%s)", ctx.Err(), l2TxID))
				return false
			case <-time.After(backoffs[attempt-1]):
			}
		}
		statusCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		status, err := o.submitter.FetchTransactionStatus(statusCtx, l2TxID)
		cancel()
		if err != nil {
			slog.Warn("L2 status poll error", "sid", sid, "l2TxId", l2TxID,
				"attempt", attempt+1, "err", err)
			continue
		}
		switch status {
		case L2StatusFailed:
			o.fail(sid, fmt.Sprintf("L2 tx FAILED on-chain (l2TxId=%s)", l2TxID))
			return false
		case L2StatusConfirmed, L2StatusProcessed:
			return true
		}
		// INCLUDED / UNKNOWN — keep polling.
		slog.Debug("L2 status not yet terminal",
			"sid", sid, "l2TxId", l2TxID, "status", status, "attempt", attempt+1)
	}
	// Deadline elapsed without terminal status. Don't mint the token.
	// Mark as forward-failed so frontend doesn't see a stuck session.
	// The L2 tx may still land eventually — an operator can recover
	// manually by inspecting Session.L2TxId in /healthz / /status.
	o.counters.ReconcileTimeouts.Add(1)
	o.fail(sid, fmt.Sprintf("L2 reconcile timed out (l2TxId=%s); inspect chain state to recover", l2TxID))
	return false
}

func (o *Orchestrator) fail(sid, reason string) {
	slog.Warn("orchestrator: session failed", "sid", sid, "reason", reason)
	o.sessions.MutateState(sid, func(s *Session) {
		if !s.State.IsTerminal() {
			s.State = StateForwardFailed
			s.ForwardError = reason
		}
	})
}
