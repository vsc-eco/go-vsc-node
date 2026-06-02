package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p"
	libpHost "github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	pubsub "github.com/libp2p/go-libp2p-pubsub"
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
//   * BroadcastRequest — publish an IsLockAttestationRequest message.
//   * Subscribe — receive IsLockAttestationResponse messages and route
//     them to the attestation collector via the onResponse callback.
//
// Lifecycle:
//
//   bcast, err := newLibp2pBroadcaster(ctx, cfg)   // creates host + connects
//   bcast.Start(onResponse)                         // begins subscribing
//   defer bcast.Close()
//   bcast.BroadcastRequest(ctx, req)                // publishes
type libp2pBroadcaster struct {
	host    libpHost.Host
	ps      *pubsub.PubSub
	topic   *pubsub.Topic
	sub     *pubsub.Subscription
	topicID string
	chainID string

	stopOnce sync.Once
	stopCh   chan struct{}
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

	// Connect to bootstrap peers. Failures here are non-fatal — gossipsub
	// will still publish to any peer that eventually connects.
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
		}
		cancel()
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
		host:    h,
		ps:      ps,
		topic:   topic,
		sub:     sub,
		topicID: topicID,
		chainID: cfg.ChainID,
		stopCh:  make(chan struct{}),
	}, nil
}

// Start begins the subscriber goroutine. Inbound responses route into
// onResponse. The goroutine stops when Close is called or ctx is done.
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
			// Drop our own messages — gossipsub echoes them locally
			// and we shouldn't deliver them as if they were peer
			// responses.
			if msg.ReceivedFrom == b.host.ID() {
				continue
			}
			var parsed wireMessage
			if err := json.Unmarshal(msg.GetData(), &parsed); err != nil {
				continue
			}
			if parsed.Type == "response" && parsed.Response != nil {
				if onResponse != nil {
					onResponse(*parsed.Response)
				}
			}
			// Requests are forwarded by the gossipsub mesh; we ignore
			// them on the IS-service side (we're a requester, not an
			// attester).
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
	Type     string                             `json:"type"`
	Request  *islock.IsLockAttestationRequest   `json:"request,omitempty"`
	Response *islock.IsLockAttestationResponse  `json:"response,omitempty"`
}
