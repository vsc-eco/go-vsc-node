package announcements

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	systemconfig "vsc-node/modules/common/system-config"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/multiformats/go-multiaddr"
)

// reachabilityAfter reports Unknown until `after` has passed, then `then`,
// like modules/p2p recording its first EvtLocalReachabilityChanged.
type reachabilityAfter struct {
	start time.Time
	after time.Duration
	then  network.Reachability
	calls atomic.Int64
}

func (r *reachabilityAfter) GetPeerId() string                   { return "" }
func (r *reachabilityAfter) GetPeerAddrs() []multiaddr.Multiaddr { return nil }
func (r *reachabilityAfter) GetPeerAddr() multiaddr.Multiaddr    { return nil }
func (r *reachabilityAfter) GetStatus() network.Reachability {
	r.calls.Add(1)
	if time.Since(r.start) < r.after {
		return network.ReachabilityUnknown
	}
	return r.then
}

func withWait(t *testing.T, d time.Duration) {
	old := startupReachabilityWait
	startupReachabilityWait = d
	t.Cleanup(func() { startupReachabilityWait = old })
}

// The mainnet failure: the event lands a few seconds after start. The startup
// announcement must wait for it and then read Public, not Unknown.
func TestStartupAnnounceWaitsForReachability(t *testing.T) {
	withWait(t, 10*time.Second)
	p := &reachabilityAfter{start: time.Now(), after: 1500 * time.Millisecond, then: network.ReachabilityPublic}
	a := &AnnouncementsManager{sconf: systemconfig.MainnetConfig(), peerInfo: p}
	a.waitForReachability(context.Background())
	if got := a.safeStatus(); got != network.ReachabilityPublic {
		t.Fatalf("after the wait the status is %v, want Public (the announcement would say enabled=false)", got)
	}
	if waited := time.Since(p.start); waited > 5*time.Second {
		t.Fatalf("waited %v for a status that arrived after 1.5s", waited)
	}
}

// A node that never hears from AutoNAT announces after the bound, as before.
func TestStartupAnnounceWaitIsBounded(t *testing.T) {
	withWait(t, 1200*time.Millisecond)
	p := &reachabilityAfter{start: time.Now(), after: time.Hour, then: network.ReachabilityPublic}
	a := &AnnouncementsManager{sconf: systemconfig.MainnetConfig(), peerInfo: p}
	start := time.Now()
	a.waitForReachability(context.Background())
	if waited := time.Since(start); waited < time.Second || waited > 4*time.Second {
		t.Fatalf("waited %v, want about the 1.2s bound", waited)
	}
}

// Known at start (or a Private result): no wait.
func TestStartupAnnounceNoWaitWhenKnown(t *testing.T) {
	withWait(t, 10*time.Second)
	for _, r := range []network.Reachability{network.ReachabilityPublic, network.ReachabilityPrivate} {
		p := &reachabilityAfter{start: time.Now(), then: r}
		a := &AnnouncementsManager{sconf: systemconfig.MainnetConfig(), peerInfo: p}
		start := time.Now()
		a.waitForReachability(context.Background())
		if waited := time.Since(start); waited > 200*time.Millisecond {
			t.Fatalf("%v known at start: waited %v", r, waited)
		}
	}
}

// Other networks keep the immediate announcement.
func TestStartupAnnounceNoWaitOffMainnet(t *testing.T) {
	withWait(t, 10*time.Second)
	for name, sc := range map[string]systemconfig.SystemConfig{
		"testnet": systemconfig.TestnetConfig(),
		"devnet":  systemconfig.DevnetConfig(),
		"mocknet": systemconfig.MocknetConfig(),
	} {
		p := &reachabilityAfter{start: time.Now(), after: time.Hour}
		a := &AnnouncementsManager{sconf: sc, peerInfo: p}
		start := time.Now()
		a.waitForReachability(context.Background())
		if waited := time.Since(start); waited > 200*time.Millisecond {
			t.Fatalf("%s: waited %v", name, waited)
		}
		if p.calls.Load() != 0 {
			t.Fatalf("%s: read reachability %d times, want 0", name, p.calls.Load())
		}
	}
}

// Shutdown during the wait returns promptly.
func TestStartupAnnounceWaitStopsOnShutdown(t *testing.T) {
	withWait(t, 10*time.Second)
	p := &reachabilityAfter{start: time.Now(), after: time.Hour}
	a := &AnnouncementsManager{sconf: systemconfig.MainnetConfig(), peerInfo: p}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(300 * time.Millisecond); cancel() }()
	start := time.Now()
	a.waitForReachability(ctx)
	if waited := time.Since(start); waited > 2*time.Second {
		t.Fatalf("waited %v after shutdown", waited)
	}
}
