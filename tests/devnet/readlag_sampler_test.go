package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// readLagSampler measures how long nodes take to read a new Hive block: every
// 50ms it records when hived first reports each block as head and when each
// node first reports it processed. At real cadence that is the live streamer's
// read lag; on fast blocks it shows how far a node trails the pacemaker. Both
// decide where, against the chain, the work a block triggers starts.
type readLagSampler struct {
	mu        sync.Mutex
	seen      map[uint64]time.Time   // block -> first sample with head >= block
	processed []map[uint64]time.Time // per node: block -> first sample with processed >= block
	stop      context.CancelFunc
	done      chan struct{}
}

const readLagSampleEvery = 50 * time.Millisecond

func startReadLagSampler(d *Devnet) *readLagSampler {
	ctx, cancel := context.WithCancel(context.Background())
	s := &readLagSampler{
		seen:      map[uint64]time.Time{},
		processed: make([]map[uint64]time.Time, d.cfg.Nodes),
		stop:      cancel,
		done:      make(chan struct{}),
	}
	for i := range s.processed {
		s.processed[i] = map[uint64]time.Time{}
	}
	rpc := d.HiveRPCEndpoint()
	client := &http.Client{Timeout: 300 * time.Millisecond}
	go func() {
		defer close(s.done)
		var head uint64
		nodeAt := make([]uint64, d.cfg.Nodes)
		tick := time.NewTicker(readLagSampleEvery) // wall: samples wall time on purpose
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			now := time.Now() // wall: the measurement
			if hi, err := getHeadBlock(rpc); err == nil && uint64(hi) > head {
				h := uint64(hi)
				s.mu.Lock()
				for b := head + 1; b <= h; b++ {
					if head > 0 {
						s.seen[b] = now
					}
				}
				s.mu.Unlock()
				head = h
			}
			for i := 0; i < d.cfg.Nodes; i++ {
				p, ok := readProcessed(client, d.quiescenceURL(i+1))
				if !ok || p <= nodeAt[i] {
					continue
				}
				s.mu.Lock()
				for b := nodeAt[i] + 1; b <= p; b++ {
					if nodeAt[i] > 0 {
						s.processed[i][b] = now
					}
				}
				s.mu.Unlock()
				nodeAt[i] = p
			}
		}
	}()
	return s
}

func readProcessed(client *http.Client, url string) (uint64, bool) {
	resp, err := client.Get(url)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	var q struct {
		Processed *uint64 `json:"processed"`
	}
	if json.NewDecoder(resp.Body).Decode(&q) != nil || q.Processed == nil {
		return 0, false
	}
	return *q.Processed, true
}

// report stops the sampler and summarizes blocks in [from, to] that hived and
// every node were seen to move past one block at a time (a sample that skipped
// blocks cannot time them): each node's lag, and per block the first and last
// node to read it.
func (s *readLagSampler) report(from, to uint64) string {
	s.stop()
	<-s.done
	s.mu.Lock()
	defer s.mu.Unlock()
	perNode := make([][]time.Duration, len(s.processed))
	var first, last, spread []time.Duration
	blocks := 0
	for b := from; b <= to; b++ {
		seen, ok := s.seen[b]
		if !ok || s.seen[b-1].Equal(seen) {
			continue // not seen, or seen in the same sample as the previous block
		}
		lags := make([]time.Duration, 0, len(s.processed))
		for i := range s.processed {
			at, ok := s.processed[i][b]
			if !ok || s.processed[i][b-1].Equal(at) {
				lags = nil
				break
			}
			lags = append(lags, at.Sub(seen))
		}
		if lags == nil {
			continue
		}
		blocks++
		for i, l := range lags {
			perNode[i] = append(perNode[i], l)
		}
		sorted := append([]time.Duration(nil), lags...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		first = append(first, sorted[0])
		last = append(last, sorted[len(sorted)-1])
		spread = append(spread, sorted[len(sorted)-1]-sorted[0])
	}
	pct := func(v []time.Duration) string {
		if len(v) == 0 {
			return "-"
		}
		v = append([]time.Duration(nil), v...)
		sort.Slice(v, func(i, j int) bool { return v[i] < v[j] })
		at := func(p int) time.Duration { return v[(len(v)-1)*p/100].Round(10 * time.Millisecond) }
		return fmt.Sprintf("%s/%s/%s", at(10), at(50), at(90))
	}
	var b strings.Builder
	fmt.Fprintf(&b, "blocks=%d (of %d-%d, sampled every %s) lag p10/p50/p90:", blocks, from, to, readLagSampleEvery)
	for i, v := range perNode {
		fmt.Fprintf(&b, " magi-%d %s", i+1, pct(v))
	}
	fmt.Fprintf(&b, " | first reader %s | last reader %s | spread %s", pct(first), pct(last), pct(spread))
	return b.String()
}
