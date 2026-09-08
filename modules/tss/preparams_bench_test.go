package tss_test

import (
	"context"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/bnb-chain/tss-lib/v3/ecdsa/keygen"
)

// envInt reads a positive integer from the environment, falling back to def.
func envInt(name string, def int) int {
	if v := os.Getenv(name); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// D6 — the PreParamsTimeout budget, measured instead of assumed.
//
// GeneratePreParams' own doc says safe-prime generation runs "15 seconds on a fast machine
// to several minutes on a loaded server", and its concurrency scales with core count. Every
// network currently leaves TssParams.PreParamsTimeout unset, so the budget is the 1-minute
// fallback everywhere. If generation routinely exceeds that on a low-core witness, the
// clean-fail becomes a node that never completes a keygen — a liveness failure that looks
// like nothing at all.
//
// This test is the instrument for setting that number. It generates pre-params at the
// concurrency a small witness would actually have and reports the distribution, so the value
// in system-config is chosen from measurements on representative hardware rather than from
// the shape of the default.
//
//	TSS_PREPARAMS_BENCH=1 TSS_PREPARAMS_CONCURRENCY=2 TSS_PREPARAMS_RUNS=5 \
//	  go test -run TestPreParamsGenerationBudget -timeout 60m ./modules/tss/
//
// Safe primes are found by rejection sampling, so the spread matters more than the mean:
// the budget has to cover the SLOW tail, not the typical case. The test reports max as well
// as median and deliberately asserts nothing about wall-clock — a timing assertion would
// fail on a busy CI box and tell you nothing about the witness you actually care about.
func TestPreParamsGenerationBudget(t *testing.T) {
	if os.Getenv("TSS_PREPARAMS_BENCH") == "" {
		t.Skip("set TSS_PREPARAMS_BENCH=1 (CPU-intensive: minutes per run)")
	}
	concurrency := envInt("TSS_PREPARAMS_CONCURRENCY", 2)
	runs := envInt("TSS_PREPARAMS_RUNS", 3)

	durations := make([]time.Duration, 0, runs)
	for i := 0; i < runs; i++ {
		start := time.Now()
		// No timeout: measure how long generation ACTUALLY takes. Capping it at the budget
		// under test would censor exactly the slow tail the budget has to cover.
		if _, err := keygen.GeneratePreParamsWithContext(context.Background(), concurrency); err != nil {
			t.Fatalf("run %d: GeneratePreParams failed: %v", i+1, err)
		}
		d := time.Since(start)
		durations = append(durations, d)
		t.Logf("run %d/%d at concurrency %d: %s", i+1, runs, concurrency, d.Round(time.Millisecond))
	}

	var total, max time.Duration
	for _, d := range durations {
		total += d
		if d > max {
			max = d
		}
	}
	mean := total / time.Duration(len(durations))
	t.Logf("PREPARAMS BUDGET concurrency=%d runs=%d mean=%s max=%s (current fallback budget: 1m)",
		concurrency, runs, mean.Round(time.Millisecond), max.Round(time.Millisecond))
	if max > time.Minute {
		t.Logf("MEASURED: the slowest run exceeded the 1m fallback. PreParamsTimeout must be "+
			"set above %s for this class of hardware, or keygen clean-fails on the slow tail.", max.Round(time.Second))
	}
}
