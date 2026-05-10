package state_engine

import (
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"vsc-node/lib/vsclog"
)

// backgroundBucketPrefixes names buckets that run on background goroutines
// rather than on the ProcessBlock critical path. They get a "bg" tag in
// LogSummary instead of a percentage of `total`, since their work happens
// concurrently and would otherwise produce >100% nonsense (e.g. an 8-worker
// prefetcher doing 90s of fetches while ProcessBlock spends 13s of wall-clock).
var backgroundBucketPrefixes = []string{"prefetch."}

func isBackgroundBucket(name string) bool {
	for _, prefix := range backgroundBucketPrefixes {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}

var profLog = vsclog.Module("se-prof")

// ProfileBucket is the aggregate of one named timing bucket within a window.
type ProfileBucket struct {
	Count   int64
	TotalNs int64
}

// Profile aggregates per-phase timings across many ProcessBlock invocations
// so a periodic summary can identify hot paths during reindex.
type Profile struct {
	mu              sync.Mutex
	buckets         map[string]*ProfileBucket
	blocksProcessed int64
	windowStart     time.Time
	windowStartBh   uint64
}

var globalProfile = &Profile{
	buckets:     make(map[string]*ProfileBucket),
	windowStart: time.Now(),
}

// GetProfile returns the process-wide profile collector.
func GetProfile() *Profile { return globalProfile }

// Record adds a single timing sample to the named bucket.
func (p *Profile) Record(name string, d time.Duration) {
	profileBucketDuration.WithLabelValues(name).Observe(d.Seconds())
	p.mu.Lock()
	b, ok := p.buckets[name]
	if !ok {
		b = &ProfileBucket{}
		p.buckets[name] = b
	}
	b.Count++
	b.TotalNs += int64(d)
	p.mu.Unlock()
}

// Measure runs fn and records its execution time under name. Use for top-level
// phases where the call shape is convenient; for fast inner sub-buckets prefer
// `start := time.Now(); ...; Record(name, time.Since(start))` to avoid the
// closure overhead in tight loops.
func (p *Profile) Measure(name string, fn func()) {
	start := time.Now()
	fn()
	p.Record(name, time.Since(start))
}

// IncBlock increments the block counter and returns the new count. The first
// call after Reset stamps the window's starting block height.
func (p *Profile) IncBlock(bh uint64) int64 {
	p.mu.Lock()
	if p.blocksProcessed == 0 {
		p.windowStartBh = bh
	}
	p.blocksProcessed++
	n := p.blocksProcessed
	p.mu.Unlock()
	return n
}

// Reset clears all buckets and starts a new window stamped at bh.
func (p *Profile) Reset(bh uint64) {
	p.mu.Lock()
	p.buckets = make(map[string]*ProfileBucket)
	p.blocksProcessed = 0
	p.windowStart = time.Now()
	p.windowStartBh = bh
	p.mu.Unlock()
}

// LogSummary emits a header line plus one info line per bucket sorted by total
// time desc. Percentages for critical-path buckets are relative to the `total`
// bucket; background buckets (parallel goroutines like the prefetcher) get a
// "bg" tag instead since they don't compete for ProcessBlock wall-clock.
// Critical-path buckets are listed first, then background buckets.
func (p *Profile) LogSummary(toBh uint64) {
	type entry struct {
		name       string
		count      int64
		ns         int64
		background bool
	}

	p.mu.Lock()
	blocks := p.blocksProcessed
	fromBh := p.windowStartBh
	elapsed := time.Since(p.windowStart)
	var totalNs int64
	if t, ok := p.buckets["total"]; ok {
		totalNs = t.TotalNs
	}
	entries := make([]entry, 0, len(p.buckets))
	for k, v := range p.buckets {
		entries = append(entries, entry{k, v.Count, v.TotalNs, isBackgroundBucket(k)})
	}
	p.mu.Unlock()

	if blocks == 0 {
		return
	}

	sort.Slice(entries, func(i, j int) bool {
		// Critical-path buckets first, each group sorted by total time desc.
		if entries[i].background != entries[j].background {
			return !entries[i].background
		}
		return entries[i].ns > entries[j].ns
	})

	profLog.Debug("window",
		"blocks", blocks,
		"elapsed_ms", elapsed.Milliseconds(),
		"from_bh", fromBh,
		"to_bh", toBh,
	)
	for _, e := range entries {
		var avgUs int64
		if e.count > 0 {
			avgUs = (e.ns / e.count) / 1000
		}
		pct := "0.00"
		if e.background {
			pct = "bg"
		} else if totalNs > 0 {
			pct = fmt.Sprintf("%.2f", float64(e.ns)/float64(totalNs)*100)
		}
		profLog.Debug("bucket",
			"name", e.name,
			"count", e.count,
			"total_ms", e.ns/int64(time.Millisecond),
			"avg_us", avgUs,
			"pct", pct,
		)
	}
}
