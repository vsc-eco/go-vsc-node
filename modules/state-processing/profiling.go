package state_engine

import (
	"sort"
	"sync"
	"time"

	"vsc-node/lib/vsclog"
)

// Indexing lifecycle phase keys for the profiler. Each key accumulates
// duration samples for one phase of block processing; the periodic summary
// log ("indexing profile", one line per phase) reports count, window-wide
// min/max/avg, and percentiles over the retained rolling window.
const (
	PhaseProcessBlock          = "processBlock"
	PhaseKeyLifecycle          = "keyLifecycle"
	PhaseVirtualOps            = "virtualOps"
	PhaseTxParse               = "txParse"
	PhaseTxParseAccountUpdate  = "txParse.accountUpdate"
	PhaseTxParseTssSign        = "txParse.tss_sign"
	PhaseTxParseTssCommitment  = "txParse.tss_commitment"
	PhaseProduceBlock          = "produceBlock"
	PhaseExecuteBatch          = "executeBatch"
	PhaseUpdateBalances        = "updateBalances"
	PhaseUpdateBalancesAccount = "updateBalances.account"
	PhaseUpdateRcMap           = "updateRcMap"
	PhaseUpdateRcMapAccount    = "updateRcMap.account"
	PhaseSaveBlockHeight       = "saveBlockHeight"
	PhaseInit                  = "init"
	PhaseFlush                 = "flush"
	PhaseDbStall               = "dbStall"
)

// PhaseExecuteBatchOp returns the per-op-type phase key for batch execution,
// e.g. "executeBatch.call", "executeBatch.transfer", "executeBatch.tss_sign".
func PhaseExecuteBatchOp(txType string) string {
	return PhaseExecuteBatch + "." + txType
}

const (
	// profilingWindowSamples bounds the per-phase ring buffer. Percentiles are
	// computed over these retained samples; min/max/avg cover the whole window
	// since the last summary emission.
	profilingWindowSamples = 4096
	// profilingSummaryLiveBlocks is the summary cadence while live-synced
	// (emitted at Info level). With a 3s Hive block this is ~50 minutes.
	profilingSummaryLiveBlocks = 1000
	// profilingSummaryCatchupBlocks is the summary cadence during catch-up
	// (emitted at Debug level), mirroring the magi-log throttle precedent.
	profilingSummaryCatchupBlocks = 100000
)

var seprofLog = vsclog.Module("seprof")

// phaseStats accumulates duration samples for one phase. The ring buffer
// retains the most recent profilingWindowSamples samples for percentile
// computation; min/max/total/count span the full window since the last
// summary emission (the window is reset on every emission).
type phaseStats struct {
	mu      sync.Mutex
	samples []time.Duration // ring buffer, grows to profilingWindowSamples then overwrites
	head    int
	total   time.Duration
	min     time.Duration
	max     time.Duration
	count   uint64
}

func (ps *phaseStats) record(d time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.count++
	ps.total += d
	if ps.count == 1 {
		ps.min, ps.max = d, d
	} else {
		if d < ps.min {
			ps.min = d
		}
		if d > ps.max {
			ps.max = d
		}
	}
	if len(ps.samples) < profilingWindowSamples {
		ps.samples = append(ps.samples, d)
		return
	}
	ps.samples[ps.head] = d
	ps.head = (ps.head + 1) % profilingWindowSamples
}

// phaseSummary is one phase's aggregated stats at emission time.
type phaseSummary struct {
	key string
	n   uint64
	min time.Duration
	max time.Duration
	avg time.Duration
	p50 time.Duration
	p95 time.Duration
	p99 time.Duration
}

// Profiler is the always-on indexing performance profiler. It is cheap (one
// monotonic-clock read per sample) and has no external API beyond the
// periodic summary log — operators read performance from the "indexing
// profile" log lines emitted by MaybeEmit.
type Profiler struct {
	mu       sync.Mutex
	phases   map[string]*phaseStats
	lastEmit uint64
}

func newProfiler() *Profiler {
	return &Profiler{phases: make(map[string]*phaseStats)}
}

// stats returns (creating if needed) the phaseStats for key.
func (pr *Profiler) stats(key string) *phaseStats {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	ps, ok := pr.phases[key]
	if !ok {
		ps = &phaseStats{}
		pr.phases[key] = ps
	}
	return ps
}

// Record adds one duration sample for a phase.
func (pr *Profiler) Record(key string, d time.Duration) {
	if pr == nil || d < 0 {
		return
	}
	pr.stats(key).record(d)
}

// Start returns a stop closure that records the elapsed duration. Use with
// `defer` only at function/block scope — Go defers do not run per loop
// iteration, so per-iteration call sites must use Record directly.
func (pr *Profiler) Start(key string) func() {
	if pr == nil {
		return func() {}
	}
	start := time.Now()
	return func() {
		pr.Record(key, time.Since(start))
	}
}

// percentile returns the p-th percentile (0..1) of a sorted sample slice
// using nearest-rank indexing.
func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	return sorted[int(p*float64(len(sorted)-1))]
}

func buildSummary(key string, ps *phaseStats) phaseSummary {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	s := phaseSummary{key: key, n: ps.count, min: ps.min, max: ps.max}
	if ps.count > 0 {
		s.avg = ps.total / time.Duration(ps.count)
	}
	sorted := make([]time.Duration, len(ps.samples))
	copy(sorted, ps.samples)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	s.p50 = percentile(sorted, 0.50)
	s.p95 = percentile(sorted, 0.95)
	s.p99 = percentile(sorted, 0.99)
	return s
}

// MaybeEmit logs the "indexing profile" summary for the window since the
// last emission once bh has advanced interval blocks past it, then resets
// all phase stats. live selects the cadence: profilingSummaryLiveBlocks at
// Info while live-synced, profilingSummaryCatchupBlocks at Debug during
// catch-up (mirrors the tssLogSync / lastMagiLogHeight throttle). Call from
// ProcessBlock on every block; nil-safe and safe to call concurrently.
func (pr *Profiler) MaybeEmit(bh uint64, live bool) {
	if pr == nil {
		return
	}
	interval := uint64(profilingSummaryCatchupBlocks)
	if live {
		interval = profilingSummaryLiveBlocks
	}
	pr.mu.Lock()
	if bh < pr.lastEmit+interval {
		pr.mu.Unlock()
		return
	}
	pr.lastEmit = bh
	summaries := make([]phaseSummary, 0, len(pr.phases))
	for key, ps := range pr.phases {
		summaries = append(summaries, buildSummary(key, ps))
	}
	pr.phases = make(map[string]*phaseStats)
	pr.mu.Unlock()

	sort.Slice(summaries, func(i, j int) bool { return summaries[i].key < summaries[j].key })
	mode := "catchup"
	if live {
		mode = "live"
	}
	seprofLog.Debug("indexing profile summary",
		"mode", mode, "windowBlocks", interval, "phases", len(summaries))
	for _, s := range summaries {
		if s.n == 0 {
			continue
		}
		seprofLog.Debug("indexing profile",
			"mode", mode,
			"phase", s.key,
			"n", s.n,
			"avg", s.avg.String(),
			"p50", s.p50.String(),
			"p95", s.p95.String(),
			"p99", s.p99.String(),
			"min", s.min.String(),
			"max", s.max.String(),
		)
	}
}
