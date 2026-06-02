package main

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// submitterHealthSnapshot is the value handleHealthz publishes via the
// SubmitterHealth hook. It MUST be cheap to read because /healthz is
// a hot probe path — production load balancers and synthetic monitors
// hit it every 1-5s.
//
// Round-5 audit R5-OP-02: the round-4 implementation did a synchronous
// 5s GraphQL probe per /healthz request with no rate limit, turning
// any moderate probe-burst into a self-DoS. R5-OP-06 noted the
// degraded flag also flipped red on a single transient GraphQL hiccup
// (no hysteresis).
//
// The fix moves the GraphQL probe to a background goroutine that
// refreshes a copy-on-write atomic snapshot on a fixed interval. The
// /healthz handler reads the snapshot lock-free. Hysteresis kicks in
// after 3 consecutive failures (mirroring DashdWatcher's threshold).
type submitterHealthSnapshot struct {
	DID              string
	BalanceHbdCents  int64
	RcRemaining      int64
	Err              error
	ConsecutiveFails int
	UpdatedAt        time.Time
}

// submitterHealthMonitor holds the snapshot pointer + the most recent
// probe state. Snapshot updates publish via atomic.Pointer so readers
// never block; the goroutine is bounded by the refresh interval, so a
// stuck upstream cannot stall /healthz.
type submitterHealthMonitor struct {
	probe   func(ctx context.Context) (string, int64, int64, error)
	snap    atomic.Pointer[submitterHealthSnapshot]
	fails   int
	refresh time.Duration
}

// newSubmitterHealthMonitor builds a monitor that probes via the given
// closure. The first probe runs synchronously so /healthz returns a
// populated snapshot immediately after server start; subsequent
// probes run on the background loop.
func newSubmitterHealthMonitor(refresh time.Duration, probe func(ctx context.Context) (string, int64, int64, error)) *submitterHealthMonitor {
	if refresh <= 0 {
		refresh = 15 * time.Second
	}
	m := &submitterHealthMonitor{
		probe:   probe,
		refresh: refresh,
	}
	return m
}

// Run blocks until ctx is cancelled, refreshing the snapshot every
// refresh interval. Safe to call from a goroutine; one call per
// monitor.
func (m *submitterHealthMonitor) Run(ctx context.Context) {
	// Initial synchronous probe with a short deadline so server
	// startup isn't gated on the upstream being reachable.
	m.tick(ctx)
	t := time.NewTicker(m.refresh)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.tick(ctx)
		}
	}
}

func (m *submitterHealthMonitor) tick(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	did, bal, rc, err := m.probe(ctx)
	if err != nil {
		m.fails++
	} else {
		m.fails = 0
	}
	snap := &submitterHealthSnapshot{
		DID:              did,
		BalanceHbdCents:  bal,
		RcRemaining:      rc,
		Err:              err,
		ConsecutiveFails: m.fails,
		UpdatedAt:        time.Now(),
	}
	m.snap.Store(snap)
	if err != nil {
		slog.Warn("submitter health probe failed",
			"err", err, "consecutiveFails", m.fails)
	}
}

// Snapshot returns the most recently observed health snapshot. Safe
// to call concurrently; returns nil before the first probe completes.
func (m *submitterHealthMonitor) Snapshot() *submitterHealthSnapshot {
	return m.snap.Load()
}
