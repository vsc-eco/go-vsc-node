package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"time"

	islock "vsc-node/modules/islock-attestation"
)

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
		collector:         cfg.Collector,
		broadcaster:       cfg.Broadcaster,
		submitter:         cfg.Submitter,
		sessions:          cfg.Sessions,
		chainID:           cfg.ChainID,
		quorumThreshold:   cfg.QuorumThreshold,
		collectTimeout:    cfg.CollectTimeout,
		submitTimeout:     cfg.SubmitTimeout,
		reconcileBackoffs: cfg.ReconcileBackoffs,
		epochFor:          cfg.EpochFor,
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

	responses := o.collector.Collect(collectCtx, txid, o.quorumThreshold, o.collectTimeout)

	// Per-sig BLS verify BEFORE aggregation (round-2 audit R2-N5).
	// Without this gate, one junk-sig response from a misbehaving peer
	// is enough to satisfy QuorumThreshold=1, the orchestrator aggregates
	// junk, submits the L2 tx, and the contract rejects after RC is
	// already spent. Verifying each sig against its claimed PubkeyHex
	// is one pairing per response; cheap compared to the L2 round-trip.
	verifiedResponses := make([]islock.IsLockAttestationResponse, 0, len(responses))
	for _, r := range responses {
		ok, err := islock.VerifyAttestation(req, r)
		if err != nil || !ok {
			slog.Warn("attestation per-sig verify failed; dropping",
				"sid", sid, "txid", txid, "validatorDID", r.ValidatorDID,
				"err", err)
			continue
		}
		verifiedResponses = append(verifiedResponses, r)
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

	submitCtx, submitCancel := context.WithTimeout(ctx, o.submitTimeout)
	defer submitCancel()
	l2TxID, err := o.submitter.SubmitMapInstantSend(submitCtx, payload)
	if err != nil {
		o.fail(sid, fmt.Sprintf("L2 submission failed: %v", err))
		return
	}

	// L2 mempool-accepted. Persist the L2 txID and transition to
	// L2_SUBMITTED — but do NOT mint the session token yet.
	// Round-2 audit D2-DESIGN-06: the spec requires waiting for
	// block-inclusion before issuing the token. Reconciler polls the
	// L2 status until terminal.
	o.sessions.MutateState(sid, func(s *Session) {
		s.State = StateL2Submitted
		s.L2TxId = l2TxID
	})

	if !o.reconcileL2(ctx, sid, txid, req.ChainId, l2TxID) {
		// reconcileL2 already set the failed state.
		return
	}

	now := time.Now()
	o.sessions.MutateState(sid, func(s *Session) {
		s.State = StateOnChain
		s.OnChainAt = &now
		// v1 session token: hex of (sid || dashTxId || chainID). This is a
		// stable opaque handle the frontend can present to Altera; Altera
		// validates it by checking the dash-mapping-contract state for
		// the bound DashDID. Production may swap this for a JWT signed by
		// the IS service's HSM/KMS once that lands.
		h := sha256.Sum256([]byte(sid + "|" + txid + "|" + req.ChainId))
		s.SessionToken = hex.EncodeToString(h[:])
	})

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
	// manually by inspecting Session.L2TxId in logs.
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
