//go:build evm_devnet

package devnet

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

// ChaosRPC stands between the bot and anvil and behaves like a free hosted L1
// RPC on a bad day: it refuses some calls (HTTP 429, 502), drops some
// connections without an answer, loses the answer to some calls it did forward
// (the node ran them; the client sees an error), answers some slowly, and
// answers some head queries from a lagging backend (an older but real answer). It never alters an
// answer otherwise. Faults are drawn from a seeded source and counted, so a wave
// can show the bot met them.
type ChaosRPC struct {
	URL      string
	upstream string
	cfg      ChaosConfig
	srv      *http.Server
	mu       sync.Mutex
	rnd      *rand.Rand
	on       bool
	counts   map[string]int
	snaps    map[string][]chaosSnap // head query -> recent answers, oldest first
}

// ChaosConfig sets each fault's probability per call (0..1).
type ChaosConfig struct {
	Seed             int64
	Refuse429        float64
	Refuse502        float64
	Drop             float64
	Lost             float64 // forwarded, then the connection is closed before the answer
	Slow             float64
	SlowMin, SlowMax time.Duration
	// StaleHead answers eth_blockNumber and eth_getBlockByNumber("latest" |
	// "safe" | "finalized") with an answer up to StaleAge old.
	StaleHead float64
	StaleAge  time.Duration
}

// MainnetWeather is a free RPC on a bad day, as phase C runs met them
// (publicnode 429s, slow answers, load-balanced backends a few blocks behind).
func MainnetWeather(seed int64) ChaosConfig {
	return ChaosConfig{Seed: seed, Refuse429: 0.08, Refuse502: 0.02, Drop: 0.01, Lost: 0.01,
		Slow: 0.10, SlowMin: 200 * time.Millisecond, SlowMax: 1500 * time.Millisecond,
		StaleHead: 0.15, StaleAge: 20 * time.Second}
}

type chaosSnap struct {
	at     time.Time
	result json.RawMessage
}

// StartChaosRPC serves on a free local port and forwards to upstream. Faults
// are on from the start; SetFaults switches them.
func StartChaosRPC(upstream string, cfg ChaosConfig) (*ChaosRPC, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	c := &ChaosRPC{URL: "http://" + ln.Addr().String(), upstream: upstream, cfg: cfg,
		rnd: rand.New(rand.NewSource(cfg.Seed)), on: true, counts: map[string]int{}, snaps: map[string][]chaosSnap{}}
	c.srv = &http.Server{Handler: c}
	go func() { _ = c.srv.Serve(ln) }()
	return c, nil
}

func (c *ChaosRPC) Stop() { _ = c.srv.Close() }

// SetFaults turns fault injection on or off (forwarding continues).
func (c *ChaosRPC) SetFaults(on bool) {
	c.mu.Lock()
	c.on = on
	c.mu.Unlock()
}

// Counts returns how many calls met each fault ("calls" is the total).
func (c *ChaosRPC) Counts() map[string]int {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]int, len(c.counts))
	for k, v := range c.counts {
		out[k] = v
	}
	return out
}

func (c *ChaosRPC) String() string {
	m := c.Counts()
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "%s=%d ", k, m[k])
	}
	return strings.TrimSpace(b.String())
}

// headKey names a head query whose answer moves with the chain ("" for any
// other call).
func headKey(method string, params []json.RawMessage) string {
	switch method {
	case "eth_blockNumber":
		return method
	case "eth_getBlockByNumber":
		if len(params) > 0 {
			var tag string
			if json.Unmarshal(params[0], &tag) == nil && (tag == "latest" || tag == "safe" || tag == "finalized") {
				parts := make([]string, len(params))
				for i, p := range params {
					parts[i] = string(p)
				}
				return method + "/" + strings.Join(parts, ",")
			}
		}
	}
	return ""
}

// fault draws this call's fault ("" for none).
func (c *ChaosRPC) fault(stale bool) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts["calls"]++
	if !c.on {
		return ""
	}
	r := c.rnd.Float64()
	for _, f := range []struct {
		name string
		p    float64
	}{{"429", c.cfg.Refuse429}, {"502", c.cfg.Refuse502}, {"drop", c.cfg.Drop}, {"lost", c.cfg.Lost}, {"slow", c.cfg.Slow}} {
		if r < f.p {
			c.counts[f.name]++
			return f.name
		}
		r -= f.p
	}
	if stale && c.rnd.Float64() < c.cfg.StaleHead {
		return "stale"
	}
	return ""
}

func (c *ChaosRPC) slowFor() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	span := c.cfg.SlowMax - c.cfg.SlowMin
	if span <= 0 {
		return c.cfg.SlowMin
	}
	return c.cfg.SlowMin + time.Duration(c.rnd.Int63n(int64(span)))
}

// staleAnswer is the oldest kept answer to key within StaleAge, if it differs
// from the newest (an answer that has not moved is not stale).
func (c *ChaosRPC) staleAnswer(key string) (json.RawMessage, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.snaps[key]
	if len(s) < 2 || bytes.Equal(s[0].result, s[len(s)-1].result) {
		return nil, false
	}
	c.counts["stale"]++
	return s[0].result, true
}

func (c *ChaosRPC) remember(key string, result json.RawMessage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now() // wall: answers age in real time, as the bot process sees them
	s := append(c.snaps[key], chaosSnap{now, append(json.RawMessage(nil), result...)})
	for len(s) > 1 && now.Sub(s[0].at) > c.cfg.StaleAge {
		s = s[1:]
	}
	c.snaps[key] = s
}

func (c *ChaosRPC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req struct {
		ID     json.RawMessage   `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	key := ""
	if json.Unmarshal(body, &req) == nil { // a batch (array) passes without a stale answer
		key = headKey(req.Method, req.Params)
	}
	f := c.fault(key != "")
	switch f {
	case "429":
		http.Error(w, "rate limited", http.StatusTooManyRequests)
		return
	case "502":
		http.Error(w, "bad gateway", http.StatusBadGateway)
		return
	case "drop":
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
		http.Error(w, "dropped", http.StatusServiceUnavailable)
		return
	case "slow":
		time.Sleep(c.slowFor()) // wall: a slow answer is slow to the real bot process
	case "stale":
		if result, ok := c.staleAnswer(key); ok {
			out, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
			return
		}
	}
	resp, err := http.Post(c.upstream, "application/json", bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	if f == "lost" {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
				return
			}
		}
	}
	if key != "" {
		var ans struct {
			Result json.RawMessage `json:"result"`
		}
		if json.Unmarshal(out, &ans) == nil && len(ans.Result) > 0 && string(ans.Result) != "null" {
			c.remember(key, ans.Result)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
}
