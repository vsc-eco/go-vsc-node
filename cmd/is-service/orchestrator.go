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
		collector:       cfg.Collector,
		broadcaster:     cfg.Broadcaster,
		submitter:       cfg.Submitter,
		sessions:        cfg.Sessions,
		chainID:         cfg.ChainID,
		quorumThreshold: cfg.QuorumThreshold,
		collectTimeout:  cfg.CollectTimeout,
		submitTimeout:   cfg.SubmitTimeout,
		epochFor:        cfg.EpochFor,
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
	rawTxHash := sha256.Sum256(rawTxBytes)
	// Dash actually uses sha256d for tx hashing; v1 uses sha256 for
	// canonical-message construction (validators do the same). Aligning
	// with sha256d is a workstream-5b follow-up — needs coordinated
	// change in modules/islock-attestation/attestation.go and the
	// contract's CanonicalAttestationMessage.

	instrHash := sha256.Sum256([]byte(sess.Instruction))

	req := islock.IsLockAttestationRequest{
		TxId:               txid,
		RawTxHashHex:       hex.EncodeToString(rawTxHash[:]),
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

	if err := o.broadcaster.BroadcastRequest(ctx, req); err != nil {
		o.fail(sid, fmt.Sprintf("broadcast failed: %v", err))
		return
	}

	collectCtx, cancel := context.WithTimeout(ctx, o.collectTimeout)
	defer cancel()
	responses := o.collector.Collect(collectCtx, txid, o.quorumThreshold, o.collectTimeout)

	if len(responses) < o.quorumThreshold {
		slog.Warn("attestation quorum not reached",
			"sid", sid, "txid", txid, "got", len(responses), "need", o.quorumThreshold)
		o.sessions.MutateState(sid, func(s *Session) {
			if s.State == StateAttesting {
				s.State = StateAttestationTimeout
				s.ForwardError = fmt.Sprintf("only %d/%d attestations collected within %s",
					len(responses), o.quorumThreshold, o.collectTimeout)
			}
		})
		return
	}

	payload := MapInstantSendPayload{
		TxId:               txid,
		RawTxHex:           rawTxHex,
		InstructionRaw:     sess.Instruction,
		InstructionHashHex: req.InstructionHashHex,
		Epoch:              req.Epoch,
		ChainId:            req.ChainId,
		Attestations:       responses,
	}

	submitCtx, submitCancel := context.WithTimeout(ctx, o.submitTimeout)
	defer submitCancel()
	if err := o.submitter.SubmitMapInstantSend(submitCtx, payload); err != nil {
		o.fail(sid, fmt.Sprintf("L2 submission failed: %v", err))
		return
	}

	now := time.Now()
	o.sessions.MutateState(sid, func(s *Session) {
		s.State = StateOnChain
		s.OnChainAt = &now
		// v1 session token: hex of (sid || dashTxId || epoch). This is a
		// stable opaque handle the frontend can present to Altera; Altera
		// validates it by checking the dash-mapping-contract state for
		// the bound DashDID. Production may swap this for a JWT signed by
		// the IS service's HSM/KMS once that lands.
		h := sha256.Sum256([]byte(sid + "|" + txid + "|" + req.ChainId))
		s.SessionToken = hex.EncodeToString(h[:])
	})

	slog.Info("session reached ON_CHAIN", "sid", sid, "txid", txid)
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
