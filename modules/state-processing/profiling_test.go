package state_engine

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProfilerPercentiles checks count/min/max/avg and nearest-rank
// percentiles on a known sample set.
func TestProfilerPercentiles(t *testing.T) {
	pr := newProfiler()
	key := "phase.x"
	for i := 1; i <= 100; i++ {
		pr.Record(key, time.Duration(i)*time.Microsecond)
	}
	sum := buildSummary(key, pr.stats(key))

	require.Equal(t, uint64(100), sum.n)
	require.Equal(t, time.Microsecond, sum.min)
	require.Equal(t, 100*time.Microsecond, sum.max)
	require.Equal(t, 50*time.Microsecond+500*time.Nanosecond, sum.avg) // (1..100)/100 = 50.5µs
	require.Equal(t, 50*time.Microsecond, sum.p50)
	require.Equal(t, 95*time.Microsecond, sum.p95)
	require.Equal(t, 99*time.Microsecond, sum.p99)

	// Single-sample edge: every percentile collapses to the sample.
	pr = newProfiler()
	pr.Record(key, 42*time.Millisecond)
	sum = buildSummary(key, pr.stats(key))
	require.Equal(t, uint64(1), sum.n)
	require.Equal(t, 42*time.Millisecond, sum.min)
	require.Equal(t, 42*time.Millisecond, sum.max)
	require.Equal(t, 42*time.Millisecond, sum.avg)
	require.Equal(t, 42*time.Millisecond, sum.p50)
	require.Equal(t, 42*time.Millisecond, sum.p95)
	require.Equal(t, 42*time.Millisecond, sum.p99)
}

// TestProfilerRingRollover verifies the ring buffer retains only the most
// recent profilingWindowSamples for percentiles while count/min/max/avg keep
// spanning the whole window since the last emission.
func TestProfilerRingRollover(t *testing.T) {
	pr := newProfiler()
	key := "phase.y"
	// 4096 fast samples (1ms) followed by 904 slow samples (10ms). The ring
	// retains the last 4096 samples = 3192x1ms + 904x10ms.
	for i := 0; i < profilingWindowSamples; i++ {
		pr.Record(key, time.Millisecond)
	}
	for i := 0; i < 904; i++ {
		pr.Record(key, 10*time.Millisecond)
	}

	ps := pr.stats(key)
	require.Equal(t, uint64(5000), ps.count)
	require.Len(t, ps.samples, profilingWindowSamples)

	sum := buildSummary(key, ps)
	require.Equal(t, time.Millisecond, sum.min)    // window-wide
	require.Equal(t, 10*time.Millisecond, sum.max) // window-wide
	require.Equal(t, time.Millisecond, sum.p50)    // retained ring median is 1ms (3192 of 4096)
	require.Equal(t, 10*time.Millisecond, sum.p95) // retained ring 95th pct is 10ms
	require.Equal(t, 10*time.Millisecond, sum.p99)
	// avg spans the whole window: (4096*1 + 904*10)ms / 5000 = 2.6272ms
	require.Equal(t, 2*time.Millisecond+627*time.Microsecond+200*time.Nanosecond, sum.avg)
}

// TestProfilerResetAfterEmit verifies an emission resets every phase so the
// next summary covers only the following window.
func TestProfilerResetAfterEmit(t *testing.T) {
	pr := newProfiler()
	pr.Record(PhaseProcessBlock, time.Millisecond)
	pr.Record(PhaseTxParse, 2*time.Millisecond)

	pr.MaybeEmit(1000, true)
	require.Empty(t, pr.phases, "phases must be reset after emission")

	// A fresh window holds exactly the post-reset samples, never the old ones.
	pr.Record(PhaseProcessBlock, 3*time.Millisecond)
	sum := buildSummary(PhaseProcessBlock, pr.stats(PhaseProcessBlock))
	require.Equal(t, uint64(1), sum.n)
	require.Equal(t, 3*time.Millisecond, sum.avg)

	// The next emission resets again.
	pr.MaybeEmit(2000, true)
	require.Empty(t, pr.phases, "phases must be reset after every emission")
}

// TestProfilerEmitCadence verifies the live (1000) and catchup (100000) block
// cadences: no emission before the threshold, exactly one at it, none after
// until the next window elapses.
func TestProfilerEmitCadence(t *testing.T) {
	pr := newProfiler()
	pr.MaybeEmit(999, true)
	require.Equal(t, uint64(0), pr.lastEmit, "must not emit before the live threshold")
	pr.MaybeEmit(1000, true)
	require.Equal(t, uint64(1000), pr.lastEmit, "must emit at the live threshold")
	pr.MaybeEmit(1999, true)
	require.Equal(t, uint64(1000), pr.lastEmit, "must not emit again mid-window")
	pr.MaybeEmit(2000, true)
	require.Equal(t, uint64(2000), pr.lastEmit, "must emit at the next live window")

	pr = newProfiler()
	pr.MaybeEmit(9999, false)
	require.Equal(t, uint64(0), pr.lastEmit, "must not emit before the catchup threshold")
	pr.MaybeEmit(100000, false)
	require.Equal(t, uint64(100000), pr.lastEmit, "must emit at the catchup threshold")
}

// TestProfilerStartClosure verifies Start returns a stop closure that records
// a positive elapsed duration into the phase.
func TestProfilerStartClosure(t *testing.T) {
	pr := newProfiler()
	stop := pr.Start(PhaseProduceBlock)
	time.Sleep(5 * time.Millisecond)
	stop()
	sum := buildSummary(PhaseProduceBlock, pr.stats(PhaseProduceBlock))
	require.Equal(t, uint64(1), sum.n)
	require.GreaterOrEqual(t, sum.avg, 5*time.Millisecond)
}

// TestProfilerNilSafety verifies all public profiler entry points are
// nil-safe so a hypothetically unwired StateEngine never panics.
func TestProfilerNilSafety(t *testing.T) {
	var pr *Profiler
	require.NotPanics(t, func() { pr.Record("x", time.Millisecond) })
	require.NotPanics(t, func() { stop := pr.Start("x"); stop() })
	require.NotPanics(t, func() { pr.MaybeEmit(1, true) })
}

// TestBlockingRetryStallHook verifies the optional stall hook reports the
// cumulative retry sleep time only when reads actually retried, and stays
// silent on a first-try success.
func TestBlockingRetryStallHook(t *testing.T) {
	attempts := 0
	var stall time.Duration
	blockingRetry("test-op", func() error {
		attempts++
		if attempts <= 2 {
			return errors.New("transient failure")
		}
		return nil
	}, func(d time.Duration) { stall = d })
	require.Equal(t, 3, attempts)
	require.GreaterOrEqual(t, stall, 300*time.Millisecond, "stall must cover both backoff sleeps")

	stall = 0
	blockingRetry("test-op", func() error { return nil }, func(d time.Duration) { stall = d })
	require.Equal(t, time.Duration(0), stall, "no stall recorded on first-try success")

	// Variadic hook must be optional — existing call sites compile unchanged.
	require.NotPanics(t, func() {
		blockingRetry("test-op", func() error { return nil })
	})
}

// TestPhaseExecuteBatchOpKey verifies the per-op-type phase key shape.
func TestPhaseExecuteBatchOpKey(t *testing.T) {
	require.Equal(t, "executeBatch.call", PhaseExecuteBatchOp("call"))
	require.Equal(t, "executeBatch.vsc.transfer", PhaseExecuteBatchOp("vsc.transfer"))
}
