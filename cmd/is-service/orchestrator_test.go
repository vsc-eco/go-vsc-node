package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	bls "github.com/protolambda/bls12-381-util"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	islock "vsc-node/modules/islock-attestation"
)

// mkValidatorKey returns a fresh BLS secret + its pubkey hex. Used by
// orchestrator tests that need real signatures so the aggregator can
// run end-to-end.
func mkValidatorKey(t *testing.T) (*bls.SecretKey, string) {
	t.Helper()
	var skBytes [32]byte
	_, err := rand.Read(skBytes[:])
	require.NoError(t, err)
	var sk bls.SecretKey
	require.NoError(t, sk.Deserialize(&skBytes))
	pk, err := bls.SkToPk(&sk)
	require.NoError(t, err)
	pkBytes := pk.Serialize()
	return &sk, hex.EncodeToString(pkBytes[:])
}

// fakeBroadcaster captures requests + can synthesize attestations
// straight into a collector to drive end-to-end tests.
type fakeBroadcaster struct {
	mu        sync.Mutex
	requests  []islock.IsLockAttestationRequest
	collector *attestationCollector
	// onBroadcast, if set, runs after the request is captured.
	onBroadcast func(islock.IsLockAttestationRequest)
	err         error
}

func (f *fakeBroadcaster) BroadcastRequest(ctx context.Context, req islock.IsLockAttestationRequest) error {
	f.mu.Lock()
	f.requests = append(f.requests, req)
	cb := f.onBroadcast
	err := f.err
	f.mu.Unlock()
	if cb != nil {
		cb(req)
	}
	return err
}

func (f *fakeBroadcaster) Requests() []islock.IsLockAttestationRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]islock.IsLockAttestationRequest, len(f.requests))
	copy(out, f.requests)
	return out
}

// fakeSubmitter captures submissions.
type fakeSubmitter struct {
	mu            sync.Mutex
	payloads      []MapInstantSendPayload
	err           error
	fakeStatus    string // empty → defaults to L2StatusConfirmed
	fakeStatusErr error
}

func (f *fakeSubmitter) SubmitMapInstantSend(ctx context.Context, p MapInstantSendPayload) (string, error) {
	f.mu.Lock()
	f.payloads = append(f.payloads, p)
	err := f.err
	f.mu.Unlock()
	if err != nil {
		return "", err
	}
	return "fake-l2-tx-" + p.Body.ChainId, nil
}

// FetchTransactionStatus returns a configurable status for tests. Default
// is "CONFIRMED" — happy path. Tests that want to exercise the
// reconciler failure path override fakeStatus.
func (f *fakeSubmitter) FetchTransactionStatus(ctx context.Context, l2TxID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fakeStatusErr != nil {
		return "", f.fakeStatusErr
	}
	if f.fakeStatus != "" {
		return f.fakeStatus, nil
	}
	return L2StatusConfirmed, nil
}

func (f *fakeSubmitter) Submissions() []MapInstantSendPayload {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]MapInstantSendPayload, len(f.payloads))
	copy(out, f.payloads)
	return out
}

// rawTxFor returns a minimal hex-encoded raw tx body for tests.
const fakeRawTxHex = "deadbeef00112233"

// fakeTxid is a stable 32-byte hex txid (64 hex chars) for tests that
// need to satisfy CanonicalSigningMessage's hex-len check.
const fakeTxid = "deadbeefcafef00d00112233445566778899aabbccddeeff00112233445566cc"

// testBackoffs is a fast L2-status-poll schedule for tests. The
// production default is 4-60s; tests use sub-millisecond steps.
var testBackoffs = []time.Duration{1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond}

func putObservedSession(t *testing.T, store *SessionStore, sid string) *Session {
	t.Helper()
	now := time.Now()
	sess := &Session{
		Sid:         sid,
		Op:          OpAuth,
		Instruction: "op=auth;sid=" + sid,
		State:       StateISObserved,
		CreatedAt:   now,
		ExpiresAt:   now.Add(30 * time.Minute),
	}
	_ = store.Put(sess)
	return sess
}

// Round-5 audit R5-COV-04 + R5-005: pin the new orchestrator
// counters so a future refactor can't silently regress the R4-007
// observability surface. Each test exercises one counter through a
// distinct path.
//
// preSubmitFlippingSubmitter never reaches SubmitMapInstantSend — it
// is wrapped by a flip-then-submit broadcaster that mutates state to
// StateExpired between Collect and Drive's pre-Submit Get(). Used to
// pin the R4-CSM-07 refusal counter deterministically (no race).
type preSubmitFlippingBroadcaster struct {
	collector *attestationCollector
	store     *SessionStore
	sid       string
	sk        *bls.SecretKey
	pkHex     string
}

func (b *preSubmitFlippingBroadcaster) BroadcastRequest(ctx context.Context, req islock.IsLockAttestationRequest) error {
	resp, err := islock.BuildResponse(req, "did:key:validator-1", b.sk)
	if err != nil {
		return err
	}
	resp.PubkeyHex = b.pkHex
	b.collector.Deliver(resp)
	// Flip state synchronously while we still hold control —
	// before Drive's Collect returns. By the time the orchestrator
	// reaches the pre-Submit Get(), state is Expired.
	b.store.MutateState(b.sid, func(s *Session) {
		s.State = StateExpired
	})
	return nil
}

func TestOrchestrator_PreSubmitRefusalIncrementsCounter(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	submitter := &fakeSubmitter{}
	sk, pkHex := mkValidatorKey(t)
	broadcaster := &preSubmitFlippingBroadcaster{
		collector: collector,
		store:     store,
		sid:       "sid-presubmit",
		sk:        sk,
		pkHex:     pkHex,
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:          store,
		Collector:         collector,
		Broadcaster:       broadcaster,
		Submitter:         submitter,
		ChainID:           "vsc-testnet",
		QuorumThreshold:   1,
		CollectTimeout:    200 * time.Millisecond,
		ReconcileBackoffs: testBackoffs,
	})

	putObservedSession(t, store, "sid-presubmit")
	orch.Drive(context.Background(), "sid-presubmit", fakeTxid, fakeRawTxHex)

	counters := orch.Counters()
	assert.Equal(t, int64(1), counters.PreSubmitRefusals,
		"PreSubmitRefusals must increment when state flips to non-Attesting before submit")
	// SubmitMapInstantSend must NOT have been called.
	assert.Empty(t, submitter.Submissions(),
		"submitter must not be called after pre-Submit guard refuses")
}

func TestOrchestrator_ReconcileTimeoutIncrementsCounter(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector}
	// fakeSubmitter returns "UNKNOWN" status forever; reconcile will time out.
	submitter := &fakeSubmitter{fakeStatus: L2StatusUnknown}
	sk, pkHex := mkValidatorKey(t)

	broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			resp, _ := islock.BuildResponse(req, "did:key:validator-1", sk)
			resp.PubkeyHex = pkHex
			collector.Deliver(resp)
		}()
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:        store,
		Collector:       collector,
		Broadcaster:     broadcaster,
		Submitter:       submitter,
		ChainID:         "vsc-testnet",
		QuorumThreshold: 1,
		CollectTimeout:  200 * time.Millisecond,
		// Tight backoffs — reconcile budget exhausts quickly.
		ReconcileBackoffs: []time.Duration{
			1 * time.Millisecond, 1 * time.Millisecond, 1 * time.Millisecond,
		},
	})

	putObservedSession(t, store, "sid-reconcile-timeout")
	orch.Drive(context.Background(), "sid-reconcile-timeout", fakeTxid, fakeRawTxHex)

	counters := orch.Counters()
	assert.Equal(t, int64(1), counters.ReconcileTimeouts,
		"ReconcileTimeouts must increment when status stays UNKNOWN")
	sess, _ := store.Get("sid-reconcile-timeout")
	assert.Equal(t, StateForwardFailed, sess.State)
}

// Round-6 audit R6-TEST-05: pin the R5-ADV-01 roster-divergence
// signal. When the expected validator set knows none of the
// libp2p responders, every drop increments AttestationVerifyDrops
// AND the orchestrator emits a structured slog.Error.
func TestOrchestrator_RosterDivergence_DropsUnknownResponders(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector}
	submitter := &fakeSubmitter{}
	sk, pkHex := mkValidatorKey(t)

	broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			resp, err := islock.BuildResponse(req, "did:key:unknown-responder", sk)
			if err != nil {
				return
			}
			resp.PubkeyHex = pkHex
			collector.Deliver(resp)
		}()
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:        store,
		Collector:       collector,
		Broadcaster:     broadcaster,
		Submitter:       submitter,
		ChainID:         "vsc-testnet",
		QuorumThreshold: 1,
		CollectTimeout:  200 * time.Millisecond,
		// Expected set knows ONLY did:key:expected — NOT the
		// responder DID. Every responder should be dropped, and
		// the divergence slog.Error should fire.
		ValidatorSetForEpoch: func(ctx context.Context, _ uint64) map[string]string {
			return map[string]string{"did:key:expected": pkHex}
		},
		ValidatorSetSource: "https://gql.test/graphql",
		ReconcileBackoffs:  testBackoffs,
	})

	putObservedSession(t, store, "sid-roster")
	orch.Drive(context.Background(), "sid-roster", fakeTxid, fakeRawTxHex)

	counters := orch.Counters()
	assert.Equal(t, int64(1), counters.AttestationVerifyDrops,
		"unknown responder must increment AttestationVerifyDrops")
	sess, _ := store.Get("sid-roster")
	assert.Equal(t, StateAttestationTimeout, sess.State,
		"all-unknown responders → no quorum → ATTESTATION_TIMEOUT")
	assert.Empty(t, submitter.Submissions(),
		"submitter must not be called when no responders pass the DID gate")
}

// Round-8 audit R8-TEST-01: pin sanitizeURLForLog against every
// pathological input class the R8-SEC-01 fix targeted. Without this
// the next refactor (e.g. accepting opaque "host:port" emit-as-is)
// silently re-opens the credential-leak hole the round-7 fix was
// supposed to close.
func TestSanitizeURLForLog(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"clean-https", "https://gql.example.org/graphql", "https://gql.example.org"},
		{"clean-http-port", "http://gql.example.org:8080/graphql", "http://gql.example.org:8080"},
		{"strip-userinfo", "https://user:pass@gql.example.org/graphql", "https://gql.example.org"},
		{"strip-query", "https://gql.example.org/graphql?token=secret", "https://gql.example.org"},
		{"strip-fragment", "https://gql.example.org/graphql#sensitive", "https://gql.example.org"},
		{"ipv6-host", "https://[fe80::1]:8080/path", "https://[fe80::1]:8080"},
		{"schemeless-with-creds", "user:pass@host:8080/api", "<redacted: missing scheme>"},
		{"host-port-only", "host:8080", "<redacted: missing scheme>"},
		// url.Parse accepts most strings; control-byte cases that
		// would otherwise emit raw must still redact.
		{"control-bytes", "https://gql.example.org/\x00leak", "<redacted: unparseable URL>"},
		// Round-9 audit R9-TEST-01: backslash-host doesn't parse
		// as a URL; verify it lands in the explicit-redacted branch
		// (and never raw, which would leak the path component).
		{"backslash-host", "http://host\\path", "<redacted: unparseable URL>"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Round-14 follow-up: bare sanitizeURLForLog wrapper was
			// deleted; call WithFlag with an empty flag to pin the
			// no-flag default-marker shape.
			got := sanitizeURLForLogWithFlag("", c.in)
			assert.Equal(t, c.want, got, "sanitizeURLForLogWithFlag(%q)", c.in)
			// Never leak userinfo (the '@' marker) regardless of
			// case classification.
			assert.NotContains(t, got, "user:pass",
				"sanitizer must never emit userinfo for input %q", c.in)
		})
	}
}

func TestOrchestrator_Counters_SnapshotShape(t *testing.T) {
	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:    NewSessionStore(time.Hour),
		Collector:   newAttestationCollector(),
		Broadcaster: &fakeBroadcaster{},
		Submitter:   &fakeSubmitter{},
		ChainID:     "vsc-testnet",
	})
	snap := orch.Counters()
	// Zero baseline — proves all four fields surface through the
	// snapshot path without panics.
	assert.Equal(t, int64(0), snap.UnresolvableOnChainCredits)
	assert.Equal(t, int64(0), snap.AttestationVerifyDrops)
	assert.Equal(t, int64(0), snap.ReconcileTimeouts)
	assert.Equal(t, int64(0), snap.PreSubmitRefusals)
}

func TestOrchestrator_HappyPath(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector}
	submitter := &fakeSubmitter{}
	sk, pkHex := mkValidatorKey(t)

	// Synthesize one attestation in response to each broadcast.
	broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			resp, err := islock.BuildResponse(req, "did:key:validator-1", sk)
			if err != nil {
				return
			}
			// Override the PubkeyHex with the test-known hex so the
			// assertion can validate it surfaces through the envelope.
			resp.PubkeyHex = pkHex
			collector.Deliver(resp)
		}()
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:        store,
		Collector:       collector,
		Broadcaster:     broadcaster,
		Submitter:       submitter,
		ChainID:         "vsc-testnet",
		QuorumThreshold: 1,
		CollectTimeout:  500 * time.Millisecond, ReconcileBackoffs: testBackoffs,
	})

	putObservedSession(t, store, "sid-x")

	orch.Drive(context.Background(), "sid-x", fakeTxid, fakeRawTxHex)

	sess, ok := store.Get("sid-x")
	require.True(t, ok)
	assert.Equal(t, StateOnChain, sess.State)
	assert.Equal(t, fakeTxid, sess.DashTxId)
	assert.NotEmpty(t, sess.SessionToken)
	assert.NotNil(t, sess.OnChainAt)

	require.Len(t, broadcaster.Requests(), 1)
	req := broadcaster.Requests()[0]
	assert.Equal(t, fakeTxid, req.TxId)
	assert.Equal(t, "vsc-testnet", req.ChainId)
	// Instruction hash must be deterministic over the instruction string.
	assert.Equal(t, 64, len(req.InstructionHashHex))

	require.Len(t, submitter.Submissions(), 1)
	sub := submitter.Submissions()[0]
	assert.Equal(t, fakeRawTxHex, sub.Body.RawTxHex)
	assert.Equal(t, "vsc-testnet", sub.Body.ChainId)
	require.Len(t, sub.Body.Attestations, 1)
	att := sub.Body.Attestations[0]
	assert.Equal(t, "did:key:validator-1", att.ValidatorDID)
	assert.Equal(t, pkHex, att.PubkeyHex, "PubkeyHex MUST flow through to the envelope")
	assert.Len(t, att.BlsSigHex, 192, "per-attestation sig is 96-byte hex")
	assert.Len(t, sub.Agg.AggSigHex, 192, "aggregate sig is also 96-byte hex")
	// Session must persist the L2 txID for /status observability.
	assert.Equal(t, "fake-l2-tx-vsc-testnet", sess.L2TxId)
}

func TestOrchestrator_TimeoutOnNoAttestations(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector} // no onBroadcast → no response
	submitter := &fakeSubmitter{}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:        store,
		Collector:       collector,
		Broadcaster:     broadcaster,
		Submitter:       submitter,
		ChainID:         "vsc-testnet",
		QuorumThreshold: 2,
		CollectTimeout:  50 * time.Millisecond,
	})

	putObservedSession(t, store, "sid-tmo")
	orch.Drive(context.Background(), "sid-tmo", fakeTxid, fakeRawTxHex)

	sess, _ := store.Get("sid-tmo")
	assert.Equal(t, StateAttestationTimeout, sess.State)
	assert.Contains(t, sess.ForwardError, "0/2")
	assert.Empty(t, submitter.Submissions(),
		"no submission when threshold not met")
}

func TestOrchestrator_SubmitFailMarksForwardFailed(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector}
	submitter := &fakeSubmitter{err: errors.New("L2 rejected")}
	sk, _ := mkValidatorKey(t)

	broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
		go func() {
			resp, err := islock.BuildResponse(req, "v1", sk)
			if err != nil {
				return
			}
			collector.Deliver(resp)
		}()
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:        store,
		Collector:       collector,
		Broadcaster:     broadcaster,
		Submitter:       submitter,
		ChainID:         "vsc-testnet",
		QuorumThreshold: 1,
		CollectTimeout:  200 * time.Millisecond, ReconcileBackoffs: testBackoffs,
		SubmitTimeout: 100 * time.Millisecond,
	})

	putObservedSession(t, store, "sid-fail")
	orch.Drive(context.Background(), "sid-fail", fakeTxid, fakeRawTxHex)

	sess, _ := store.Get("sid-fail")
	assert.Equal(t, StateForwardFailed, sess.State)
	assert.Contains(t, sess.ForwardError, "L2 rejected")
}

func TestOrchestrator_BroadcastFailMarksForwardFailed(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector, err: errors.New("p2p down")}
	submitter := &fakeSubmitter{}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions: store, Collector: collector, Broadcaster: broadcaster,
		Submitter: submitter, ChainID: "vsc-testnet",
	})

	putObservedSession(t, store, "sid-bcast")
	orch.Drive(context.Background(), "sid-bcast", fakeTxid, fakeRawTxHex)

	sess, _ := store.Get("sid-bcast")
	assert.Equal(t, StateForwardFailed, sess.State)
	assert.Contains(t, sess.ForwardError, "p2p down")
}

func TestOrchestrator_InvalidRawTxHexFails(t *testing.T) {
	store := NewSessionStore(time.Hour)
	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:    store,
		Collector:   newAttestationCollector(),
		Broadcaster: &fakeBroadcaster{},
		Submitter:   &fakeSubmitter{},
		ChainID:     "vsc-testnet",
	})

	putObservedSession(t, store, "sid-bad")
	orch.Drive(context.Background(), "sid-bad", "tx", "not-hex!!!")

	sess, _ := store.Get("sid-bad")
	assert.Equal(t, StateForwardFailed, sess.State)
	assert.Contains(t, sess.ForwardError, "invalid rawTxHex")
}

func TestOrchestrator_SessionNotObservedIsNoOp(t *testing.T) {
	store := NewSessionStore(time.Hour)
	broadcaster := &fakeBroadcaster{}
	submitter := &fakeSubmitter{}
	orch := NewOrchestrator(OrchestratorConfig{
		Sessions:    store,
		Collector:   newAttestationCollector(),
		Broadcaster: broadcaster,
		Submitter:   submitter,
		ChainID:     "vsc-testnet",
	})

	now := time.Now()
	_ = store.Put(&Session{
		Sid: "sid-stuck", Op: OpAuth, State: StateWaitingForIS,
		CreatedAt: now, ExpiresAt: now.Add(time.Hour),
	})

	orch.Drive(context.Background(), "sid-stuck", fakeTxid, fakeRawTxHex)
	assert.Empty(t, broadcaster.Requests(),
		"Drive must not broadcast for sessions not in IS_OBSERVED")
	assert.Empty(t, submitter.Submissions())
}

func TestOrchestrator_UnknownSessionIsNoOp(t *testing.T) {
	store := NewSessionStore(time.Hour)
	broadcaster := &fakeBroadcaster{}
	orch := NewOrchestrator(OrchestratorConfig{
		Sessions: store, Collector: newAttestationCollector(),
		Broadcaster: broadcaster, Submitter: &fakeSubmitter{}, ChainID: "vsc-testnet",
	})

	orch.Drive(context.Background(), "nonexistent", fakeTxid, fakeRawTxHex)
	assert.Empty(t, broadcaster.Requests())
}

// TestOrchestrator_NoAwaiterLeakOnHappyPath — round-2 audit R2-002.
// After Drive returns successfully, no awaiter entries should remain
// in the collector map. Without the symmetric defer Cancel, every
// session leaks one awaiter + buffered channel monotonically.
func TestOrchestrator_NoAwaiterLeakOnHappyPath(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector}
	submitter := &fakeSubmitter{}
	sk, pkHex := mkValidatorKey(t)

	broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
		go func() {
			time.Sleep(10 * time.Millisecond)
			resp, err := islock.BuildResponse(req, "did:key:v1", sk)
			if err != nil {
				return
			}
			resp.PubkeyHex = pkHex
			collector.Deliver(resp)
		}()
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions: store, Collector: collector, Broadcaster: broadcaster,
		Submitter: submitter, ChainID: "vsc-testnet",
		QuorumThreshold: 1, CollectTimeout: 500 * time.Millisecond, ReconcileBackoffs: testBackoffs,
	})

	putObservedSession(t, store, "sid-no-leak")
	orch.Drive(context.Background(), "sid-no-leak", fakeTxid, fakeRawTxHex)

	collector.mu.Lock()
	defer collector.mu.Unlock()
	assert.Empty(t, collector.awaiters,
		"awaiter map must be empty after Drive returns; got %d leaked entries",
		len(collector.awaiters))
}

// TestOrchestrator_NoAwaiterLeakOnFailurePaths — same guard for each
// non-happy-path return.
func TestOrchestrator_NoAwaiterLeakOnFailurePaths(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeBroadcaster, *fakeSubmitter, *bls.SecretKey, string)
	}{
		{"broadcast-fail", func(b *fakeBroadcaster, s *fakeSubmitter, _ *bls.SecretKey, _ string) {
			b.err = errors.New("p2p down")
		}},
		{"submit-fail", func(b *fakeBroadcaster, s *fakeSubmitter, sk *bls.SecretKey, pkHex string) {
			s.err = errors.New("L2 down")
			b.onBroadcast = func(req islock.IsLockAttestationRequest) {
				go func() {
					time.Sleep(10 * time.Millisecond)
					r, _ := islock.BuildResponse(req, "v1", sk)
					r.PubkeyHex = pkHex
					b.collector.Deliver(r)
				}()
			}
		}},
		{"quorum-timeout", func(b *fakeBroadcaster, s *fakeSubmitter, _ *bls.SecretKey, _ string) {
			// no onBroadcast → no responses ever → timeout
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := NewSessionStore(time.Hour)
			collector := newAttestationCollector()
			broadcaster := &fakeBroadcaster{collector: collector}
			submitter := &fakeSubmitter{}
			sk, pkHex := mkValidatorKey(t)
			tc.setup(broadcaster, submitter, sk, pkHex)

			orch := NewOrchestrator(OrchestratorConfig{
				Sessions: store, Collector: collector, Broadcaster: broadcaster,
				Submitter: submitter, ChainID: "vsc-testnet",
				QuorumThreshold: 1,
				CollectTimeout:  100 * time.Millisecond,
				SubmitTimeout:   100 * time.Millisecond,
			})

			putObservedSession(t, store, "sid-fail-"+tc.name)
			orch.Drive(context.Background(), "sid-fail-"+tc.name, fakeTxid, fakeRawTxHex)

			collector.mu.Lock()
			leaked := len(collector.awaiters)
			collector.mu.Unlock()
			assert.Equal(t, 0, leaked,
				"awaiter map must be empty after Drive failure (%s); got %d leaked entries",
				tc.name, leaked)
		})
	}
}

// TestOrchestrator_ReconcileL2_Failed — audit D2-DESIGN-06. When the
// L2 status comes back FAILED, the session MUST end in FORWARD_FAILED
// and NO sessionToken must be minted.
func TestOrchestrator_ReconcileL2_Failed(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector}
	submitter := &fakeSubmitter{fakeStatus: L2StatusFailed}
	sk, pkHex := mkValidatorKey(t)

	broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
		go func() {
			resp, _ := islock.BuildResponse(req, "v1", sk)
			resp.PubkeyHex = pkHex
			collector.Deliver(resp)
		}()
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions: store, Collector: collector, Broadcaster: broadcaster,
		Submitter: submitter, ChainID: "vsc-testnet",
		QuorumThreshold: 1, CollectTimeout: 200 * time.Millisecond,
		ReconcileBackoffs: testBackoffs,
	})

	putObservedSession(t, store, "sid-l2-fail")
	orch.Drive(context.Background(), "sid-l2-fail", fakeTxid, fakeRawTxHex)

	sess, _ := store.Get("sid-l2-fail")
	assert.Equal(t, StateForwardFailed, sess.State,
		"FAILED L2 status MUST mark FORWARD_FAILED, not ON_CHAIN")
	assert.Empty(t, sess.SessionToken,
		"FAILED L2 status MUST NOT mint sessionToken (audit D2-DESIGN-06)")
	assert.NotEmpty(t, sess.L2TxId,
		"L2TxId must be persisted even on failure for operator triage")
}

// TestOrchestrator_PerSigVerifyDropsJunk — audit R2-N5. A response
// with a syntactically-valid sig over the WRONG message must be
// dropped by the per-sig verify gate, never reach AggregateSignatures.
func TestOrchestrator_PerSigVerifyDropsJunk(t *testing.T) {
	store := NewSessionStore(time.Hour)
	collector := newAttestationCollector()
	broadcaster := &fakeBroadcaster{collector: collector}
	submitter := &fakeSubmitter{}
	sk, pkHex := mkValidatorKey(t)

	broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
		go func() {
			// Sign a DIFFERENT request so the sig is valid by itself
			// but doesn't verify against THIS request.
			fakeReq := req
			fakeReq.Epoch = 99999
			junkSig, _ := islock.Sign(fakeReq, sk)
			collector.Deliver(islock.IsLockAttestationResponse{
				TxId:         req.TxId,
				ValidatorDID: "did:key:junk",
				PubkeyHex:    pkHex,
				Epoch:        req.Epoch,
				BlsSigHex:    junkSig,
			})
		}()
	}

	orch := NewOrchestrator(OrchestratorConfig{
		Sessions: store, Collector: collector, Broadcaster: broadcaster,
		Submitter: submitter, ChainID: "vsc-testnet",
		QuorumThreshold: 1, CollectTimeout: 200 * time.Millisecond,
		ReconcileBackoffs: testBackoffs,
	})

	putObservedSession(t, store, "sid-junk")
	orch.Drive(context.Background(), "sid-junk", fakeTxid, fakeRawTxHex)

	sess, _ := store.Get("sid-junk")
	assert.Equal(t, StateAttestationTimeout, sess.State,
		"junk sig MUST be dropped by per-sig verify and session timeout — never aggregated and submitted")
	assert.Empty(t, submitter.Submissions(),
		"junk responses MUST NOT reach the submitter (audit R2-N5)")
}

// TestOrchestrator_SessionTokenIsRandom — audit SEC-8 (R15) regression:
// the SessionToken MUST be derived from crypto/rand, NOT from the
// public (sid, txid, chainID) tuple. Two sessions with identical
// inputs must mint DIFFERENT tokens.
//
// (The pre-SEC-8 implementation minted token = sha256(sid || txid
// || chainID); anyone scraping a /status URL could compute the token
// locally and bypass TLS confidentiality.)
func TestOrchestrator_SessionTokenIsRandom(t *testing.T) {
	mkOrch := func() *Orchestrator {
		store := NewSessionStore(time.Hour)
		collector := newAttestationCollector()
		broadcaster := &fakeBroadcaster{collector: collector}
		submitter := &fakeSubmitter{}
		sk, _ := mkValidatorKey(t)
		broadcaster.onBroadcast = func(req islock.IsLockAttestationRequest) {
			go func() {
				resp, err := islock.BuildResponse(req, "v1", sk)
				if err != nil {
					return
				}
				collector.Deliver(resp)
			}()
		}
		return NewOrchestrator(OrchestratorConfig{
			Sessions: store, Collector: collector, Broadcaster: broadcaster,
			Submitter: submitter, ChainID: "vsc-testnet",
			QuorumThreshold: 1, CollectTimeout: 200 * time.Millisecond, ReconcileBackoffs: testBackoffs,
		})
	}

	orch1 := mkOrch()
	putObservedSession(t, orch1.sessions, "sid-tok")
	orch1.Drive(context.Background(), "sid-tok", fakeTxid, fakeRawTxHex)
	sess1, _ := orch1.sessions.Get("sid-tok")
	require.NotEmpty(t, sess1.SessionToken)
	assert.Len(t, sess1.SessionToken, 64, "token is 32-byte hex (256 bits of entropy)")
	_, err := hex.DecodeString(sess1.SessionToken)
	require.NoError(t, err, "token must be valid hex")

	// Same inputs (sid, txid, chainID) → DIFFERENT token. This is the
	// SEC-8 regression assertion: pre-fix the two tokens would match
	// because sha256(sid || txid || chainID) is deterministic.
	orch2 := mkOrch()
	putObservedSession(t, orch2.sessions, "sid-tok")
	orch2.Drive(context.Background(), "sid-tok", fakeTxid, fakeRawTxHex)
	sess2, _ := orch2.sessions.Get("sid-tok")
	require.NotEmpty(t, sess2.SessionToken)
	assert.NotEqual(t, sess1.SessionToken, sess2.SessionToken,
		"SessionToken MUST be random (audit SEC-8); identical (sid, txid, "+
			"chainID) inputs MUST produce different tokens or an attacker "+
			"who scrapes /status URLs can compute the token locally")
}
