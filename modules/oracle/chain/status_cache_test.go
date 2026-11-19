package chain

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// statusRelay is a relayer whose height call can be made to fail or to hang
// until released, counting the calls.
type statusRelay struct {
	mockChainRelay
	calls   atomic.Int32
	height  atomic.Uint64
	fail    atomic.Bool
	release chan struct{} // non-nil: the call waits for it
}

func (r *statusRelay) GetLatestValidHeight() (chainState, error) {
	r.calls.Add(1)
	if r.release != nil {
		<-r.release
	}
	if r.fail.Load() {
		return chainState{}, errors.New("connection refused")
	}
	return chainState{blockHeight: r.height.Load()}, nil
}

func withStatusTiming(t *testing.T, every, wait time.Duration) {
	t.Helper()
	oe, ow := chainStatusRefreshEvery, chainStatusWait
	chainStatusRefreshEvery, chainStatusWait = every, wait
	t.Cleanup(func() { chainStatusRefreshEvery, chainStatusWait = oe, ow })
}

func statusOf(t *testing.T, st []ChainStatus, symbol string) *uint64 {
	t.Helper()
	for _, s := range st {
		if s.Symbol == symbol {
			return s.BlockHeight
		}
	}
	t.Fatalf("%s not listed in %v", symbol, st)
	return nil
}

// 18x: a chain RPC that hangs (bitcoind down: ~22 s of retries) no longer
// holds the request; the chain is listed without a height, and however many
// requests arrive, one refresh runs.
func TestChainStatus_HangingRPCDoesNotHoldTheRequest(t *testing.T) {
	withStatusTiming(t, time.Hour, 50*time.Millisecond)
	btc := &statusRelay{mockChainRelay: mockChainRelay{symbol: "BTC", contractId: "vsc1btc"}, release: make(chan struct{})}
	defer close(btc.release)
	o := &ChainOracle{chainRelayers: map[string]chainRelay{"BTC": btc}}

	var wg sync.WaitGroup
	start := time.Now()
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if h := statusOf(t, o.GetAllChainStatuses(), "BTC"); h != nil {
				t.Errorf("height %d while the RPC hangs", *h)
			}
		}()
	}
	wg.Wait()
	if d := time.Since(start); d > time.Second {
		t.Fatalf("20 requests took %v with the RPC hanging; each must return within chainStatusWait", d)
	}
	if n := btc.calls.Load(); n != 1 {
		t.Fatalf("%d RPC calls for 20 concurrent requests, want 1", n)
	}
}

// A healthy RPC answers the first request, later requests inside the interval
// make no call, and after the interval the new height is served.
func TestChainStatus_HealthyRPCIsServedAndRefreshedAfterTheInterval(t *testing.T) {
	withStatusTiming(t, 80*time.Millisecond, time.Second)
	btc := &statusRelay{mockChainRelay: mockChainRelay{symbol: "BTC", contractId: "vsc1btc"}}
	btc.height.Store(900_000)
	dash := &statusRelay{mockChainRelay: mockChainRelay{symbol: "DASH"}} // registered, not configured
	o := &ChainOracle{chainRelayers: map[string]chainRelay{"BTC": btc, "DASH": dash}}

	st := o.GetAllChainStatuses()
	if h := statusOf(t, st, "BTC"); h == nil || *h != 900_000 {
		t.Fatalf("first request: BTC height %v, want 900000", h)
	}
	if h := statusOf(t, st, "DASH"); h != nil || dash.calls.Load() != 0 {
		t.Fatalf("unconfigured DASH: height %v, %d calls; want none", h, dash.calls.Load())
	}
	btc.height.Store(900_001)
	if h := statusOf(t, o.GetAllChainStatuses(), "BTC"); h == nil || *h != 900_000 || btc.calls.Load() != 1 {
		t.Fatalf("inside the interval: height %v after %d calls, want the cached 900000 and 1 call", h, btc.calls.Load())
	}
	time.Sleep(100 * time.Millisecond)
	if h := statusOf(t, o.GetAllChainStatuses(), "BTC"); h == nil || *h != 900_001 {
		t.Fatalf("after the interval: height %v, want 900001", h)
	}
}

// A chain whose RPC fails in a refresh loses its height: an earlier height is
// not served as current.
func TestChainStatus_FailedRefreshClearsTheHeight(t *testing.T) {
	withStatusTiming(t, 10*time.Millisecond, time.Second)
	btc := &statusRelay{mockChainRelay: mockChainRelay{symbol: "BTC", contractId: "vsc1btc"}}
	btc.height.Store(5)
	o := &ChainOracle{chainRelayers: map[string]chainRelay{"BTC": btc}}
	if h := statusOf(t, o.GetAllChainStatuses(), "BTC"); h == nil {
		t.Fatal("no height from a healthy RPC")
	}
	btc.fail.Store(true)
	time.Sleep(20 * time.Millisecond)
	if h := statusOf(t, o.GetAllChainStatuses(), "BTC"); h != nil {
		t.Fatalf("height %d served after the RPC failed", *h)
	}
}

// A relayer that panics does not take the node down and the cache recovers.
func TestChainStatus_PanickingRelayerIsContained(t *testing.T) {
	withStatusTiming(t, 10*time.Millisecond, time.Second)
	o := &ChainOracle{chainRelayers: map[string]chainRelay{"BTC": &panicRelay{mockChainRelay{symbol: "BTC", contractId: "vsc1btc"}}}}
	if h := statusOf(t, o.GetAllChainStatuses(), "BTC"); h != nil {
		t.Fatalf("height %d from a panicking relayer", *h)
	}
	time.Sleep(20 * time.Millisecond)
	_ = o.GetAllChainStatuses() // a later refresh starts: the first one released its slot
	o.statusCache.mu.Lock()
	at := o.statusCache.at
	o.statusCache.mu.Unlock()
	if at.IsZero() {
		t.Fatal("the panicking refresh never finished")
	}
}

type panicRelay struct{ mockChainRelay }

func (p *panicRelay) GetLatestValidHeight() (chainState, error) { panic("relayer bug") }
