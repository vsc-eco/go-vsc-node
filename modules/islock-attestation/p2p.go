package islock_attestation

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p-pubsub"
	"github.com/libp2p/go-libp2p/core/peer"

	libp2p "vsc-node/modules/p2p"
)

// Workstream 4 follow-up: p2p service skeleton.
//
// This file pins the gossipsub topic + message-routing pattern that the
// validator-side subscriber and the IS-Service-side requester will share.
// Implements the PubSubServiceParams[p2pMessage] interface used by the
// existing TSS/oracle/block-producer modules (see modules/tss/p2p.go for
// the reference pattern and the workstream-0 scoping spike for the
// design rationale).
//
// Live wiring landed in commits 3412c573 (validator-side, cmd/vsc-node/
// islock_wiring.go: AttestationSigner + DashdPoller-backed
// MemoryReader) and the IS-service-side broadcaster
// (cmd/is-service/broadcaster_libp2p.go: requests only; never signs).
// Audit R15-CONS-06 retired the earlier "ZMQ subscriber" wording —
// validators now poll dashd via HTTP RPC.

// p2pTopic is the gossipsub topic for the lazy-attestation protocol.
// Versioned so future schema changes can roll out via /v2 without
// breaking older peers.
const p2pTopic = "/islock-attestation/v1"

// p2pMessage is the union of request + response on the same topic.
// Mirrors the modules/tss/p2p.go pattern.
type p2pMessage struct {
	Type     string                     `json:"type"` // "request" or "response"
	Request  *IsLockAttestationRequest  `json:"request,omitempty"`
	Response *IsLockAttestationResponse `json:"response,omitempty"`
}

// AttestationSigner is the validator-side dependency: when an attestation
// request arrives, the service checks memory and if found, builds a
// signed response using this signer's BLS consensus key. The IS Service
// side passes a no-op signer (requests only; never signs).
type AttestationSigner interface {
	// ValidatorDID returns the signer's did:key:... reference, used in
	// the response's ValidatorDID field.
	ValidatorDID() string
	// PubkeyHex returns the validator's 48-byte BLS pubkey, hex-encoded
	// (96 chars). Round-2 audit `R2-001` — the broadcaster + contract
	// both require this field; without it every legitimate validator
	// response is silently dropped on the IS-service ingress.
	PubkeyHex() string
	// Sign produces a BLS signature over CanonicalSigningMessage(req).
	// Implementations typically wrap a *bls.SecretKey held in the
	// validator's identity config.
	Sign(req IsLockAttestationRequest) (string, error)
}

// MemoryReader exposes the read side of an IsLockMemory. Validators
// back this with the DashdPoller's RPC-fed IsLockMemory; the IS
// Service passes nil and doesn't sign attestation requests it receives
// (it's a consumer, not an attester).
type MemoryReader interface {
	Lookup(txid string) (rawTxHex string, rawTxHash []byte, ok bool)
}

// ResponseCallback fires when a IsLockAttestationResponse arrives for a
// txid we (the IS Service) previously requested. Implementations
// typically deliver into a per-txid buffered channel so the request
// flow can collect N-of-M responses.
type ResponseCallback func(resp IsLockAttestationResponse)

// Service is the gossipsub-backed islock-attestation node-side handler.
// Construct one per validator (and one per IS Service, but with signer=nil).
type Service struct {
	signer       AttestationSigner   // nil on IS-Service side
	memory       MemoryReader        // nil on IS-Service side
	chainID      string              // included in every signed message
	onResponse   ResponseCallback    // nil on validator side
	rateLimits   *requestRateLimiter // bounds per-peer attestation cost

	// WIRING NEEDED: assigned in NewService once the libp2p.PubSubService is built.
	svc libp2p.PubSubService[p2pMessage]
}

// NewService constructs a fresh attestation service. Pass signer + memory
// (validator side) or signer=nil/memory=nil + onResponse (IS Service side).
//
// chainID disambiguates mainnet/testnet in signed messages.
//
// WIRING NEEDED: this is the call site each consumer needs to make. The
// underlying libp2p.NewPubSubService(host, params, topic) wiring is
// straightforward once the consumer has its libp2p host available.
func NewService(
	chainID string,
	signer AttestationSigner,
	memory MemoryReader,
	onResponse ResponseCallback,
) *Service {
	return &Service{
		signer:     signer,
		memory:     memory,
		chainID:    chainID,
		onResponse: onResponse,
		rateLimits: newRequestRateLimiter(),
	}
}

// Start binds a constructed libp2p.PubSubService[p2pMessage] to this
// service. After Start, BroadcastRequest can publish, and HandleMessage
// callbacks delivered by the underlying pubsub run with svc available
// for replies via the SendFunc passed to HandleMessage.
//
// Construction site (validator or IS Service):
//
//	svcParams := islock_attestation.NewService(chainID, signer, memory, onResp)
//	pubsubSvc, err := libp2p.NewPubSubService(p2pServer, svcParams)
//	if err != nil { return err }
//	svcParams.Start(pubsubSvc)
func (s *Service) Start(svc libp2p.PubSubService[p2pMessage]) {
	s.svc = svc
}

// BroadcastRequest is the IS-Service-side primitive: publish a request
// for attestation on a specific (txid, rawTxHash, instruction-hash, epoch).
// Validators that have it in their memory respond; others silently ignore.
//
// Callers collect responses via the onResponse callback registered at
// NewService time. Use a per-txid waitgroup or channel to know when
// quorum is reached.
//
// ctx is currently unused — libp2p.PubSubService.Send takes the service's
// own context. Kept on the signature so callers can wire cancellation
// once Send-with-ctx lands in the underlying pubsub.
func (s *Service) BroadcastRequest(ctx context.Context, req IsLockAttestationRequest) error {
	_ = ctx
	if s.svc == nil {
		return fmt.Errorf("p2p service not yet wired; call Start() first")
	}
	return s.svc.Send(p2pMessage{Type: "request", Request: &req})
}

// ===== libp2p.PubSubServiceParams[p2pMessage] interface =====

// Topic returns the gossipsub topic name.
func (s *Service) Topic() string { return p2pTopic }

// ValidateMessage rejects messages that don't make sense before the
// expensive HandleMessage path runs. Following the TSS pattern, we
// filter for malformed structure here and let HandleMessage do
// content-specific work.
func (s *Service) ValidateMessage(ctx context.Context, from peer.ID, raw *pubsub.Message, msg p2pMessage) bool {
	switch msg.Type {
	case "request":
		if msg.Request == nil || msg.Request.TxId == "" || msg.Request.ChainId == "" {
			return false
		}
		if msg.Request.ChainId != s.chainID {
			return false
		}
		// Per-peer rate limit so a noisy validator can't flood the topic.
		if !s.rateLimits.allow(from.String()) {
			return false
		}
		return true
	case "response":
		if msg.Response == nil || msg.Response.TxId == "" || msg.Response.BlsSigHex == "" {
			return false
		}
		// Apply the same per-peer rate limit to RESPONSE messages.
		// Without this, a misbehaving validator could flood the topic
		// with type=response messages with no per-peer cap — audit
		// `gossipsub-response-broadcast-flood`.
		if !s.rateLimits.allow(from.String()) {
			return false
		}
		return true
	}
	return false
}

// ParseMessage decodes the wire format. JSON for now to match TSS pattern.
func (Service) ParseMessage(data []byte) (p2pMessage, error) {
	var m p2pMessage
	err := json.Unmarshal(data, &m)
	return m, err
}

// SerializeMessage is the inverse of ParseMessage.
func (Service) SerializeMessage(msg p2pMessage) []byte {
	out, _ := json.Marshal(msg)
	return out
}

// HandleRawMessage is called for raw inbound pubsub messages. Most
// modules just leave this as a no-op and let HandleMessage do the work
// via the framework's parse path. We follow that convention.
func (Service) HandleRawMessage(ctx context.Context, rawMsg *pubsub.Message, send libp2p.SendFunc[p2pMessage]) error {
	return nil
}

// HandleMessage dispatches to per-side handlers based on message type.
// Validators care about requests (sign + respond); IS Service cares
// about responses (deliver to the awaiting requester).
func (s *Service) HandleMessage(
	ctx context.Context,
	from peer.ID,
	msg p2pMessage,
	send libp2p.SendFunc[p2pMessage],
) error {
	switch msg.Type {
	case "request":
		s.handleRequest(ctx, *msg.Request, send)
	case "response":
		if s.onResponse != nil {
			s.onResponse(*msg.Response)
		}
	}
	return nil
}

// handleRequest is the validator-side path: look up in memory, sign if
// found, send response. Silent on miss — no negative response (avoid
// side channel that leaks "I never saw this lock" timing info).
func (s *Service) handleRequest(
	ctx context.Context,
	req IsLockAttestationRequest,
	send libp2p.SendFunc[p2pMessage],
) {
	if s.memory == nil || s.signer == nil {
		// Not a validator — silently ignore the request.
		return
	}
	rawTxHex, _, ok := s.memory.Lookup(req.TxId)
	if !ok {
		return
	}
	_ = rawTxHex
	// TODO (audit SEC-5): defense-in-depth — verify req.RawTxHashHex
	// matches sha256d(memory[req.TxId].rawTxHex), and re-confirm
	// instantlock=true via a fresh getrawtransaction (the poller
	// already filters on instantlock when admitting, but the cached
	// entry could be stale by the time an attestation request lands).
	// Without this the validator signs over requester-supplied hashes,
	// burning CPU on contract-rejected forgeries and creating a
	// forgery primitive if a future contract change relaxes its own
	// recompute step.

	sigHex, err := s.signer.Sign(req)
	if err != nil {
		// Signing failed — silently drop. The requester will time out
		// or collect quorum from other validators.
		return
	}
	resp := IsLockAttestationResponse{
		TxId:         req.TxId,
		ValidatorDID: s.signer.ValidatorDID(),
		// Round-2 audit R2-001: PubkeyHex MUST be populated. The
		// IS-service-side broadcaster + the contract both reject
		// responses with empty/wrong-length PubkeyHex; without this
		// the entire fast-path is silently dead.
		PubkeyHex: s.signer.PubkeyHex(),
		Epoch:     req.Epoch,
		BlsSigHex: sigHex,
	}
	if send != nil {
		send(p2pMessage{Type: "response", Response: &resp})
	}
}

// ===== rate limiter (validator-side) =====

// requestRateLimiter caps how many messages a single peer can post on
// the topic per window. Prevents a single (compromised or buggy) peer
// from spamming attestation requests OR responses at the validator set
// — audit `gossipsub-response-broadcast-flood` was about asymmetric
// limiting, so we now apply allow() to BOTH branches in
// ValidateMessage.
//
// Concurrency: go-libp2p-pubsub dispatches non-inline topic validators
// in fresh goroutines per message, so requestRateLimiter.state is
// accessed concurrently and MUST be guarded by a mutex. Unsynchronized
// map writes trigger Go's runtime throw which bypasses defer/recover —
// audit `rate-limiter-no-mutex-concurrent-map-panic`.
type requestRateLimiter struct {
	mu            sync.Mutex
	state         map[string]*requestBucket
	maxPerWindow  int
	window        time.Duration
	maxPeers      int
}

type requestBucket struct {
	count    int
	windowAt time.Time
}

func newRequestRateLimiter() *requestRateLimiter {
	return &requestRateLimiter{
		state:        make(map[string]*requestBucket),
		maxPerWindow: 100, // 100 messages / minute per peer is generous
		window:       time.Minute,
		maxPeers:     10_000,
	}
}

func (r *requestRateLimiter) allow(peerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if b, ok := r.state[peerID]; ok {
		if now.Sub(b.windowAt) > r.window {
			b.count = 0
			b.windowAt = now
		}
		if b.count >= r.maxPerWindow {
			return false
		}
		b.count++
		return true
	}
	if len(r.state) >= r.maxPeers {
		// Memory cap — refuse to track new peer, fail-closed for them.
		return false
	}
	r.state[peerID] = &requestBucket{count: 1, windowAt: now}
	return true
}

// Compile-time check that Service implements the params interface.
var _ libp2p.PubSubServiceParams[p2pMessage] = &Service{}
