package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
	libpHost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/multiformats/go-multiaddr"

	islock "vsc-node/modules/islock-attestation"
)

// libp2pBroadcaster is a minimal libp2p gossipsub client for the
// islock-attestation topic.
//
// Unlike modules/p2p's full P2PServer (which is opinionated about being a
// validator participant — DHT bootstrap, witness peer set, block-sync
// gating), this is a thin client: connect to a configured set of
// bootstrap peers, join the gossipsub topic, and publish + subscribe.
//
// The IS service uses this to:
//
//   - BroadcastRequest — publish an IsLockAttestationRequest message.
//   - Subscribe — receive IsLockAttestationResponse messages and route
//     them to the attestation collector via the onResponse callback.
//
// Lifecycle:
//
//	bcast, err := newLibp2pBroadcaster(ctx, cfg)   // creates host + connects
//	bcast.Start(onResponse)                         // begins subscribing
//	defer bcast.Close()
//	bcast.BroadcastRequest(ctx, req)                // publishes
type libp2pBroadcaster struct {
	host    libpHost.Host
	ps      *pubsub.PubSub
	topic   *pubsub.Topic
	sub     *pubsub.Subscription
	topicID string
	chainID string

	// peerLimiter throttles inbound responses per peer. Mirrors
	// modules/islock-attestation's validator-side limiter — same shape,
	// IS-service side. Without it any peer can flood our collector
	// (audit `libp2p-broadcaster-no-response-validation`).
	peerLimiter *broadcasterRateLimiter

	// bootstrapConnectedCount: how many bootstrap peers handshook at
	// startup. Exposed via BootstrapConnectedCount() — used only for
	// the startup degraded-mode check. Runtime peer health uses the
	// live ConnectedPeerCount() / MeshPeerCount() (audit R2-N6).
	bootstrapConnectedCount int

	stopOnce sync.Once
	stopCh   chan struct{}
}

// broadcasterRateLimiter caps responses per (gossipsub) peer per
// minute. Concurrency: sub.Next runs single-threaded so technically
// the mutex is unneeded today, but the validate-then-deliver split
// makes it cheap insurance against future refactors.
type broadcasterRateLimiter struct {
	mu       sync.Mutex
	state    map[string]*broadcasterBucket
	maxPer   int
	window   time.Duration
	maxPeers int
}

type broadcasterBucket struct {
	count    int
	windowAt time.Time
}

func newBroadcasterRateLimiter() *broadcasterRateLimiter {
	return &broadcasterRateLimiter{
		state:    make(map[string]*broadcasterBucket),
		maxPer:   60, // 60 responses / minute per peer — generous: typical session generates 1 response per validator
		window:   time.Minute,
		maxPeers: 10_000,
	}
}

func (r *broadcasterRateLimiter) allow(peerID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	if b, ok := r.state[peerID]; ok {
		if now.Sub(b.windowAt) > r.window {
			b.count = 0
			b.windowAt = now
		}
		if b.count >= r.maxPer {
			return false
		}
		b.count++
		return true
	}
	if len(r.state) >= r.maxPeers {
		return false
	}
	r.state[peerID] = &broadcasterBucket{count: 1, windowAt: now}
	return true
}

// libp2pBroadcasterConfig configures the broadcaster.
type libp2pBroadcasterConfig struct {
	// ChainID is the VSC chain id; it's included in every published
	// IsLockAttestationRequest so validators on other chains drop it.
	ChainID string
	// TopicPrefix is the PubSubTopicPrefix (e.g. "/vsc/mainnet" or
	// "/vsc/testnet"). Matches modules/common/system-config's
	// PubSubTopicPrefix() exactly — drift would put us on the wrong
	// gossip channel.
	TopicPrefix string
	// BootstrapPeers is a list of libp2p multiaddrs we connect to on
	// startup. Operators provide these as the active witness fleet's
	// public p2p endpoints. Without at least one we'll never reach
	// the network — the constructor logs but doesn't fail (lets the
	// IS service start in degraded mode).
	BootstrapPeers []string
	// ListenAddrs is the multiaddr(s) we listen on. Defaults to
	// /ip4/0.0.0.0/tcp/0 + /ip6/::/tcp/0 (random port). For
	// container deployments operators may want a fixed port for
	// NAT-traversal predictability.
	ListenAddrs []string
}

// newLibp2pBroadcaster constructs a broadcaster, opens a host, joins
// the topic. Caller must call Start(onResponse) to begin subscribing.
func newLibp2pBroadcaster(ctx context.Context, cfg libp2pBroadcasterConfig) (*libp2pBroadcaster, error) {
	if cfg.ChainID == "" {
		return nil, fmt.Errorf("ChainID required")
	}
	if cfg.TopicPrefix == "" {
		return nil, fmt.Errorf("TopicPrefix required (e.g. /vsc/testnet)")
	}

	listenAddrs := cfg.ListenAddrs
	if len(listenAddrs) == 0 {
		listenAddrs = []string{"/ip4/0.0.0.0/tcp/0", "/ip6/::/tcp/0"}
	}

	opts := []libp2p.Option{
		libp2p.ListenAddrStrings(listenAddrs...),
		libp2p.DefaultMuxers,
		libp2p.DefaultSecurity,
		libp2p.DefaultTransports,
	}
	h, err := libp2p.New(opts...)
	if err != nil {
		return nil, fmt.Errorf("create libp2p host: %w", err)
	}

	// Connect to bootstrap peers. Count successes so the caller can
	// fail-fast on zero (audit `libp2p-zero-peers-silent-degraded-start`
	// — startup must not silently succeed with no mesh members).
	connectedCount := 0
	for _, addr := range cfg.BootstrapPeers {
		ma, err := multiaddr.NewMultiaddr(addr)
		if err != nil {
			slog.Warn("invalid bootstrap peer multiaddr", "addr", addr, "err", err)
			continue
		}
		ai, err := peer.AddrInfoFromP2pAddr(ma)
		if err != nil {
			slog.Warn("multiaddr missing /p2p/<id> suffix", "addr", addr, "err", err)
			continue
		}
		connCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		if err := h.Connect(connCtx, *ai); err != nil {
			slog.Warn("failed to connect to bootstrap peer", "addr", addr, "err", err)
		} else {
			slog.Info("connected to bootstrap peer", "peer", ai.ID.String())
			connectedCount++
		}
		cancel()
	}
	if connectedCount == 0 && len(cfg.BootstrapPeers) > 0 {
		// Caller asked for bootstrap peers but none connected. Refuse to
		// start the IS service in this state — every session would time
		// out in ATTESTATION_TIMEOUT and operators would only learn from
		// user complaints.
		_ = h.Close()
		return nil, fmt.Errorf("libp2p broadcaster: none of %d configured bootstrap peers connected — refusing to start in degraded mode",
			len(cfg.BootstrapPeers))
	}

	ps, err := pubsub.NewGossipSub(ctx, h)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("create gossipsub: %w", err)
	}

	topicID := cfg.TopicPrefix + "/islock-attestation/v1"
	topic, err := ps.Join(topicID)
	if err != nil {
		_ = h.Close()
		return nil, fmt.Errorf("join topic %s: %w", topicID, err)
	}

	sub, err := topic.Subscribe()
	if err != nil {
		_ = topic.Close()
		_ = h.Close()
		return nil, fmt.Errorf("subscribe topic %s: %w", topicID, err)
	}

	return &libp2pBroadcaster{
		host:                    h,
		ps:                      ps,
		topic:                   topic,
		sub:                     sub,
		topicID:                 topicID,
		chainID:                 cfg.ChainID,
		peerLimiter:             newBroadcasterRateLimiter(),
		bootstrapConnectedCount: connectedCount,
		stopCh:                  make(chan struct{}),
	}, nil
}

// BootstrapConnectedCount returns the count of peers connected during
// startup. Stable; meant for startup logging only. For runtime health,
// use ConnectedPeerCount() / MeshPeerCount().
func (b *libp2pBroadcaster) BootstrapConnectedCount() int { return b.bootstrapConnectedCount }

// ConnectedPeerCount returns the LIVE number of connected libp2p
// peers. Round-2 audit R2-N6: the original implementation returned a
// cached boot-time count, so /healthz reported "ok" even after rolling
// witness restarts dropped every peer.
func (b *libp2pBroadcaster) ConnectedPeerCount() int {
	if b.host == nil {
		return 0
	}
	return len(b.host.Network().Peers())
}

// MeshPeerCount returns the live count of peers in the
// islock-attestation gossipsub mesh. Mesh membership is what actually
// matters for attestation delivery — a peer connected via libp2p but
// not subscribed to our topic is effectively absent.
func (b *libp2pBroadcaster) MeshPeerCount() int {
	if b.ps == nil {
		return 0
	}
	return len(b.ps.ListPeers(b.topicID))
}

// Start begins the subscriber goroutine. Inbound responses route into
// onResponse — but only after the broadcaster validates them:
//
//   - drop our own echoed messages
//   - drop non-response types
//   - drop responses with malformed PubkeyHex / BlsSigHex (length-only;
//     full BLS verify is the contract's authority)
//   - apply a per-peer rate-limit on response volume so a single
//     misbehaving validator can't flood our collector
//
// Audit `libp2p-broadcaster-no-response-validation`. Without these
// gates, any libp2p peer could publish forged response messages with
// matching TxId + unique fake ValidatorDIDs to fill the collector's
// buffer, causing the orchestrator to submit junk that the contract
// rejects only after the operator pays RC.
func (b *libp2pBroadcaster) Start(ctx context.Context, onResponse func(islock.IsLockAttestationResponse)) {
	go func() {
		for {
			msg, err := b.sub.Next(ctx)
			if err != nil {
				select {
				case <-b.stopCh:
					return
				default:
					if ctx.Err() != nil {
						return
					}
					slog.Debug("subscription error", "err", err)
					return
				}
			}
			// Drop our own echoed messages.
			if msg.ReceivedFrom == b.host.ID() {
				continue
			}
			var parsed wireMessage
			if err := json.Unmarshal(msg.GetData(), &parsed); err != nil {
				slog.Debug("dropped malformed gossipsub message",
					"peer", msg.ReceivedFrom.String(), "err", err)
				continue
			}
			if parsed.Type != "response" || parsed.Response == nil {
				// Requests are forwarded by the gossipsub mesh; we ignore
				// them on the IS-service side (we're a requester, not an
				// attester).
				continue
			}
			r := parsed.Response
			if len(r.PubkeyHex) != 96 || len(r.BlsSigHex) != 192 ||
				r.TxId == "" || r.ValidatorDID == "" {
				slog.Debug("dropped malformed attestation response",
					"peer", msg.ReceivedFrom.String(), "txid", r.TxId,
					"pubkeyLen", len(r.PubkeyHex), "sigLen", len(r.BlsSigHex))
				continue
			}
			if !b.peerLimiter.allow(msg.ReceivedFrom.String()) {
				slog.Debug("dropped response: per-peer rate limit",
					"peer", msg.ReceivedFrom.String(), "txid", r.TxId)
				continue
			}
			if onResponse != nil {
				onResponse(*r)
			}
		}
	}()
}

// BroadcastRequest implements Broadcaster. Publishes the request onto
// the islock-attestation topic.
func (b *libp2pBroadcaster) BroadcastRequest(ctx context.Context, req islock.IsLockAttestationRequest) error {
	msg := wireMessage{Type: "request", Request: &req}
	data, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("marshal request: %w", err)
	}
	if err := b.topic.Publish(ctx, data); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// Close shuts down the subscriber + libp2p host.
func (b *libp2pBroadcaster) Close() error {
	var err error
	b.stopOnce.Do(func() {
		close(b.stopCh)
		if b.sub != nil {
			b.sub.Cancel()
		}
		if b.topic != nil {
			_ = b.topic.Close()
		}
		if b.host != nil {
			err = b.host.Close()
		}
	})
	return err
}

// wireMessage mirrors modules/islock-attestation/p2p.go's p2pMessage —
// kept private here so the IS service doesn't depend on the unexported
// type, but the JSON shape is identical.
type wireMessage struct {
	Type     string                            `json:"type"`
	Request  *islock.IsLockAttestationRequest  `json:"request,omitempty"`
	Response *islock.IsLockAttestationResponse `json:"response,omitempty"`
}
