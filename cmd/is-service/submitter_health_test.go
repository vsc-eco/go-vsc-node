package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Round-6 audit R6-TEST-03: pin the submitterHealthMonitor concurrency
// surface (atomic snapshot, 3-fail hysteresis counter, probe timeout,
// panic-recover). The probe is closure-injected, so all cases run
// without GraphQL or network.

func TestSubmitterHealthMonitor_SnapshotNilBeforeFirstTick(t *testing.T) {
	m := newSubmitterHealthMonitor(time.Hour, func(ctx context.Context) (string, int64, int64, error) {
		return "did:test", 100, 200, nil
	})
	assert.Nil(t, m.Snapshot(), "Snapshot before Run must be nil")
}

func TestSubmitterHealthMonitor_TickIncrementsFailsOnError(t *testing.T) {
	calls := atomic.Int32{}
	m := newSubmitterHealthMonitor(time.Hour, func(ctx context.Context) (string, int64, int64, error) {
		calls.Add(1)
		return "", 0, 0, errors.New("flap")
	})
	ctx := context.Background()
	m.tick(ctx)
	m.tick(ctx)
	m.tick(ctx)
	snap := m.Snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 3, snap.ConsecutiveFails)
	assert.Error(t, snap.Err)
}

func TestSubmitterHealthMonitor_SuccessResetsFails(t *testing.T) {
	phase := atomic.Int32{}
	m := newSubmitterHealthMonitor(time.Hour, func(ctx context.Context) (string, int64, int64, error) {
		if phase.Load() == 0 {
			return "", 0, 0, errors.New("transient")
		}
		return "did:ok", 50, 80, nil
	})
	ctx := context.Background()
	m.tick(ctx)
	m.tick(ctx)
	assert.Equal(t, 2, m.Snapshot().ConsecutiveFails)
	phase.Store(1)
	m.tick(ctx)
	snap := m.Snapshot()
	assert.Equal(t, 0, snap.ConsecutiveFails, "success must reset fail counter")
	assert.NoError(t, snap.Err)
	assert.Equal(t, "did:ok", snap.DID)
}

func TestSubmitterHealthMonitor_HysteresisThresholdConsistent(t *testing.T) {
	// Round-6 audit R6-CORR-02: the submitterDegradedFailThreshold
	// constant must be >= 1 (degraded after >=N consecutive fails).
	// Pin it so a future bump to 0 silently flipping the gate is
	// caught.
	assert.GreaterOrEqual(t, submitterDegradedFailThreshold, 1)
	assert.LessOrEqual(t, submitterDegradedFailThreshold, 10,
		"keep threshold tight enough that RC-exhaustion surfaces within ~minutes")
}

func TestSubmitterHealthMonitor_ProbePanic_RecoversAndIncrementsFails(t *testing.T) {
	// Round-6 audit R6-OP-05: the monitor must survive a panic in the
	// probe closure and surface it in the snapshot.
	calls := atomic.Int32{}
	m := newSubmitterHealthMonitor(time.Hour, func(ctx context.Context) (string, int64, int64, error) {
		n := calls.Add(1)
		if n == 2 {
			panic("simulated GraphQL parser blow-up")
		}
		return "did:test", 10, 20, nil
	})
	ctx := context.Background()
	m.tick(ctx) // ok
	assert.Equal(t, 0, m.Snapshot().ConsecutiveFails)
	m.tick(ctx) // panics
	snap := m.Snapshot()
	require.NotNil(t, snap)
	assert.Equal(t, 1, snap.ConsecutiveFails)
	require.Error(t, snap.Err)
	assert.Contains(t, snap.Err.Error(), "panic")
	// Subsequent ticks still work — monitor wasn't killed.
	m.tick(ctx)
	assert.Equal(t, 0, m.Snapshot().ConsecutiveFails)
}

func TestSubmitterHealthMonitor_ProbeTimeoutBounded(t *testing.T) {
	// A probe that ignores ctx.Done() should still return within the
	// 5s timeout window (the tick's deadline). We use a probe that
	// sleeps 8s + checks ctx — the tick should return before 8s
	// elapses thanks to its own 5s deadline + the probe's ctx-check.
	start := time.Now()
	m := newSubmitterHealthMonitor(time.Hour, func(ctx context.Context) (string, int64, int64, error) {
		select {
		case <-ctx.Done():
			return "", 0, 0, ctx.Err()
		case <-time.After(8 * time.Second):
			return "did:never", 0, 0, nil
		}
	})
	m.tick(context.Background())
	elapsed := time.Since(start)
	assert.Less(t, elapsed, 7*time.Second,
		"tick must respect the 5s probe deadline (got %s)", elapsed)
	snap := m.Snapshot()
	require.NotNil(t, snap)
	assert.Error(t, snap.Err, "ctx.Err must surface as the snapshot error")
}

func TestSubmitterHealthMonitor_RunExitsOnCtxDone(t *testing.T) {
	m := newSubmitterHealthMonitor(100*time.Millisecond, func(ctx context.Context) (string, int64, int64, error) {
		return "did:test", 0, 0, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Run did not exit after ctx cancel")
	}
}
