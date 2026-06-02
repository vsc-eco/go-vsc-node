package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
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
// after submitterDegradedFailThreshold consecutive failures.
type submitterHealthSnapshot struct {
	DID              string
	BalanceHbdCents  int64
	RcRemaining      int64
	Err              error
	ConsecutiveFails int
	UpdatedAt        time.Time
}

// submitterDegradedFailThreshold is the consecutive-fail count above
// which /healthz flips the submitter to degraded. Round-6 audit
// R6-CORR-02 wired this threshold through the consumer chain; the
// pre-R6 code dropped ConsecutiveFails at the callback boundary and
// flipped degraded on the first non-nil err. Chosen to align with
// the DashdWatcher's >= 5 escalation but stay tighter so RC-exhaustion
// still surfaces within ~45s (3 fails × 15s refresh).
const submitterDegradedFailThreshold = 3

// errSubmitterWarmup is the sentinel error the SubmitterHealth callback
// returns BEFORE the first probe completes (Snapshot() == nil). The
// /healthz handler distinguishes warmup from a real probe failure and
// does not flip degraded for warmup. Round-6 audit R6-CORR-03.
var errSubmitterWarmup = errors.New("submitter health probe warming up")

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
// closure. Run() does an initial probe before entering the ticker
// loop; until Run is called, Snapshot returns nil.
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
	// Round-6 audit R6-OP-05: defensive panic-recover. The probe is
	// supplied by the caller (production wires it to
	// SubmitterL2.FetchSubmitterHealth) — a future refactor that
	// introduces panic-prone parsing (map index, interface assertion)
	// would otherwise crash the IS service process. recover here
	// keeps the monitor alive, increments the fail counter, and
	// records the panic in the snapshot for /healthz to surface.
	defer func() {
		if r := recover(); r != nil {
			m.fails++
			snap := &submitterHealthSnapshot{
				Err:              fmt.Errorf("submitter health probe panic: %v", r),
				ConsecutiveFails: m.fails,
				UpdatedAt:        time.Now(),
			}
			m.snap.Store(snap)
			slog.Error("submitter health probe panicked",
				"recovered", r, "stack", string(debug.Stack()),
				"consecutiveFails", m.fails)
		}
	}()
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
