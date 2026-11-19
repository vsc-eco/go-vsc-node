package chain

import (
	"sync"
	"time"
)

// Chain statuses for localNodeInfo (POST-LAUNCH 18x, LOCALNODEINFO-BTC-RPC-HANG).
//
// localNodeInfo listed each configured chain's latest valid height by calling
// the chain's RPC on the request. btcd's rpcclient retries a refused
// connection 10 times with a growing backoff (~22 s) and allows each attempt a
// minute, so on a node whose bitcoind is down or absent every localNodeInfo
// took 22 s or more, even for a caller that only asked for the epoch (the
// resolver builds the whole object): the EVM bot's health check timed out and
// failed over every time, and a public endpoint held a goroutine per request.
// The heights now come from a cache: a request starts a refresh when the last
// one is older than chainStatusRefreshEvery and none is running (one refresh
// at a time, however many requests), waits for it at most chainStatusWait,
// and returns what the last finished refresh found. A chain whose RPC failed
// in that refresh has no height, as before; a height is never carried over
// from an earlier refresh.
var (
	chainStatusRefreshEvery = 10 * time.Second
	chainStatusWait         = 2 * time.Second
)

// ChainStatus holds the block height status of a single chain relayer.
type ChainStatus struct {
	Symbol      string
	BlockHeight *uint64 // nil if unreachable or not configured
}

type chainStatusCache struct {
	mu      sync.Mutex
	heights map[string]uint64 // symbol -> latest valid height in the last refresh
	at      time.Time         // when the last refresh finished; zero before the first
	running chan struct{}     // closed when the running refresh ends; nil when none runs
}

// GetAllChainStatuses returns the block height status for every registered chain.
func (c *ChainOracle) GetAllChainStatuses() []ChainStatus {
	s := &c.statusCache
	s.mu.Lock()
	wait := s.running
	if wait == nil && (s.at.IsZero() || time.Since(s.at) >= chainStatusRefreshEvery) {
		wait = make(chan struct{})
		s.running = wait
		go c.refreshChainStatuses(wait)
	}
	s.mu.Unlock()
	if wait != nil {
		t := time.NewTimer(chainStatusWait)
		select {
		case <-wait:
		case <-t.C:
		}
		t.Stop()
	}
	return c.chainStatusSnapshot()
}

// refreshChainStatuses asks every configured chain for its latest valid height
// and replaces the cache with the answers.
func (c *ChainOracle) refreshChainStatuses(done chan struct{}) {
	heights := make(map[string]uint64)
	defer func() {
		// The request path recovered a panicking relayer (gqlgen does); a
		// background refresh must not take the node down instead.
		if r := recover(); r != nil && c.logger != nil {
			c.logger.Error("chain status refresh panicked", "panic", r)
		}
		s := &c.statusCache
		s.mu.Lock()
		s.heights, s.at, s.running = heights, time.Now(), nil
		s.mu.Unlock()
		close(done)
	}()
	for symbol, chain := range c.chainRelayers {
		if chain.ContractId() == "" {
			continue
		}
		if state, err := chain.GetLatestValidHeight(); err == nil {
			heights[symbol] = state.blockHeight
		}
	}
}

func (c *ChainOracle) chainStatusSnapshot() []ChainStatus {
	s := &c.statusCache
	s.mu.Lock()
	defer s.mu.Unlock()
	statuses := make([]ChainStatus, 0, len(c.chainRelayers))
	for symbol := range c.chainRelayers {
		cs := ChainStatus{Symbol: symbol}
		if h, ok := s.heights[symbol]; ok {
			cs.BlockHeight = &h
		}
		statuses = append(statuses, cs)
	}
	return statuses
}
