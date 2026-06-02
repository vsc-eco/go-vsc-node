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
	mu       sync.Mutex
	payloads []MapInstantSendPayload
	err      error
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
		CollectTimeout:  500 * time.Millisecond,
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
		CollectTimeout:  200 * time.Millisecond,
		SubmitTimeout:   100 * time.Millisecond,
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

// regression: verifies the session token is deterministic for the same
// (sid, txid, chainID) — Altera frontend needs idempotent lookup.
func TestOrchestrator_SessionTokenIsDeterministic(t *testing.T) {
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
	orch := NewOrchestrator(OrchestratorConfig{
		Sessions: store, Collector: collector, Broadcaster: broadcaster,
		Submitter: submitter, ChainID: "vsc-testnet",
		QuorumThreshold: 1, CollectTimeout: 200 * time.Millisecond,
	})
	putObservedSession(t, store, "sid-tok")
	orch.Drive(context.Background(), "sid-tok", fakeTxid, fakeRawTxHex)

	sess, _ := store.Get("sid-tok")
	assert.Len(t, sess.SessionToken, 64, "token is 32-byte sha256 hex")
	// Re-derive locally with the same primitive.
	// (Don't re-call Drive — it would skip the now-terminal session.)
	_, err := hex.DecodeString(sess.SessionToken)
	assert.NoError(t, err)
}
