package devnet

// Unit tests for the FastBlocks pacemaker, against a fake hived and fake node
// quiescence probes. No Docker. Each guard of the integrity contract in
// pacemaker.go is triggered deliberately and must hold.

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	pmTestMinGap = 10 * time.Millisecond
	pmTestMaxGap = 150 * time.Millisecond
)

// fakeHived serves the four JSON-RPC calls the pacemaker makes.
type fakeHived struct {
	mu           sync.Mutex
	head         uint64
	genErr       bool
	witnessKey   string
	editTrueSeen bool
	genCalls     []time.Time
	srv          *httptest.Server
}

func newFakeHived(t *testing.T, head uint64) *fakeHived {
	f := &fakeHived{head: head, witnessKey: "STM-initminer-key"}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeHived) serve(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	defer f.mu.Unlock()
	switch req.Method {
	case "database_api.get_dynamic_global_properties":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"head_block_number":%d}}`, f.head)
	case "debug_node_api.debug_generate_blocks":
		var p struct {
			Count        int  `json:"count"`
			EditIfNeeded bool `json:"edit_if_needed"`
		}
		_ = json.Unmarshal(req.Params, &p)
		if p.EditIfNeeded {
			f.editTrueSeen = true
		}
		if f.genErr || p.EditIfNeeded {
			fmt.Fprint(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32003,"message":"refused"}}`)
			return
		}
		f.head += uint64(p.Count)
		f.genCalls = append(f.genCalls, time.Now())
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"blocks":%d}}`, p.Count)
	case "database_api.list_witnesses":
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":1,"result":{"witnesses":[{"owner":"initminer","signing_key":%q}]}}`, f.witnessKey)
	default:
		http.Error(w, "unknown method", http.StatusBadRequest)
	}
}

func (f *fakeHived) Head() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head
}

func (f *fakeHived) set(fn func(*fakeHived)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	fn(f)
}

func (f *fakeHived) gens() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.genCalls...)
}

// fakeNode serves GET /devnet/quiescence, by default idle and caught up with the
// fake hived's head.
type fakeNode struct {
	mu         sync.Mutex
	hived      *fakeHived
	headPoll   int64 // ms; newFakeNode sets 100
	blkPoll    int64 // ms; newFakeNode sets 50
	strSleep   int64 // ms; newFakeNode sets 10
	checkpoint int64 // ms; newFakeNode sets 25
	peers      int   // connected peers; newPMHarness sets all the others
	lag        uint64
	async      int64
	ceremony   int
	signing    int
	bg         int64
	bp, ep     bool
	gw, orc    int64
	txSeen     int64
	atts       map[string][]string
	omit       string // leave this field out of the report
	hits       int    // probes answered
	down       bool   // drop the connection: indistinguishable from a stopped node
	status     int    // non-zero: answer with this HTTP status
	raw        string
	srv        *httptest.Server
}

func newFakeNode(t *testing.T, h *fakeHived) *fakeNode {
	n := &fakeNode{hived: h, headPoll: 100, blkPoll: 50, strSleep: 10, checkpoint: 25}
	n.srv = httptest.NewServer(http.HandlerFunc(n.serve))
	t.Cleanup(n.srv.Close)
	return n
}

func (n *fakeNode) serve(w http.ResponseWriter, r *http.Request) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.hits++
	if n.down {
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				conn.Close()
				return
			}
		}
	}
	if n.status != 0 {
		http.Error(w, "nope", n.status)
		return
	}
	if n.raw != "" {
		_, _ = w.Write([]byte(n.raw))
		return
	}
	head := n.hived.Head()
	processed := head
	if n.lag <= head {
		processed = head - n.lag
	}
	atts := n.atts
	if atts == nil {
		atts = map[string][]string{}
	}
	report := map[string]any{
		"head_poll_ms":     n.headPoll,
		"block_poll_ms":    n.blkPoll,
		"stream_sleep_ms":  n.strSleep,
		"checkpoint_ms":    n.checkpoint,
		"peers":            n.peers,
		"processed":        processed,
		"async_ticks":      n.async,
		"tss_ceremony":     n.ceremony,
		"tss_signing":      n.signing,
		"tss_background":   n.bg,
		"bp_busy":          n.bp,
		"ep_busy":          n.ep,
		"gateway_bg":       n.gw,
		"oracle_bg":        n.orc,
		"tx_seen":          n.txSeen,
		"tss_attestations": atts,
	}
	delete(report, n.omit)
	_ = json.NewEncoder(w).Encode(report)
}

func (n *fakeNode) set(fn func(*fakeNode)) {
	n.mu.Lock()
	defer n.mu.Unlock()
	fn(n)
}

func (n *fakeNode) url() string { return n.srv.URL + "/devnet/quiescence" }

// pmHarness runs a pacemaker against fakes; failures go to a local sink, never
// the package registry (TestMain would otherwise fail the run).
type pmHarness struct {
	t         *testing.T
	hived     *fakeHived
	nodes     []*fakeNode
	pm        *pacemaker
	mu        sync.Mutex
	recorded  []string
	held      string
	leadSink  *atomic.Int64 // nil: a sink local to the harness
	unarmed   bool          // start without arming (boot)
	wake      chan struct{} // pacemakerConfig.wake (nil: none)
	maxGap    time.Duration // 0: pmTestMaxGap
	heldProbe time.Duration // 0: pmHeldProbeInterval
	blockLog  io.Writer     // pacemakerConfig.blockLog (nil: none)
}

func newPMHarness(t *testing.T, nodes int) *pmHarness {
	h := &pmHarness{t: t, hived: newFakeHived(t, 100)}
	for i := 0; i < nodes; i++ {
		n := newFakeNode(t, h.hived)
		n.peers = nodes - 1
		h.nodes = append(h.nodes, n)
	}
	return h
}

func (h *pmHarness) setHeld(why string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held = why
}

func (h *pmHarness) start(probes ...string) {
	if h.leadSink == nil {
		h.leadSink = new(atomic.Int64)
	}
	if probes == nil {
		for _, n := range h.nodes {
			probes = append(probes, n.url())
		}
	}
	h.pm = newPacemaker(pacemakerConfig{
		rpc:       h.hived.srv.URL,
		probes:    probes,
		debugKey:  "5JtestKey",
		minGap:    pmTestMinGap,
		maxGap:    cmp.Or(h.maxGap, pmTestMaxGap),
		heldProbe: h.heldProbe,
		blockLog:  h.blockLog,
		wake:      h.wake,
		held: func() string {
			h.mu.Lock()
			defer h.mu.Unlock()
			return h.held
		},
		lead: h.leadSink,
		record: func(msg string) {
			h.mu.Lock()
			h.recorded = append(h.recorded, msg)
			h.mu.Unlock()
		},
	})
	if !h.unarmed {
		h.pm.arm()
	}
	h.pm.start()
}

func (h *pmHarness) halt() []string { return h.pm.halt() }

func (h *pmHarness) waitActivated() {
	deadline := time.Now().Add(3 * time.Second)
	for !h.pm.activated.Load() {
		if time.Now().After(deadline) {
			h.t.Fatal("pacemaker never activated")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (h *pmHarness) recordedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.recorded)
}

func containsAny(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

func TestPacemakerAllIdleProducesEarlyBlocks(t *testing.T) {
	h := newPMHarness(t, 3)
	h.start()
	time.Sleep(600 * time.Millisecond)
	failures := h.halt()
	early, capped := h.pm.early.Load(), h.pm.capped.Load()
	if len(failures) != 0 || h.pm.safe.Load() {
		t.Fatalf("unexpected integrity failures: %v", failures)
	}
	if early < 10 || capped != 0 {
		t.Fatalf("all nodes idle: want many early blocks and none capped, got early=%d capped=%d", early, capped)
	}
}

// A node one block behind the head is caught up: the streamer processes block N
// once N+1 exists (streamerHeadLag). Seen live: requiring processed >= head held
// every block to the cap.
func TestPacemakerTreatsStreamerLagAsCaughtUp(t *testing.T) {
	h := newPMHarness(t, 3)
	for _, n := range h.nodes {
		// The real streamer's lag, as a literal: configuring the fake from
		// streamerHeadLag would make this test agree with any value of it.
		n.set(func(n *fakeNode) { n.lag = 1 })
	}
	h.start()
	time.Sleep(500 * time.Millisecond)
	failures := h.halt()
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if e := h.pm.early.Load(); e < 10 {
		t.Fatalf("nodes at the streamer's normal one-block lag: want early blocks, got %d", e)
	}
}

// A node that polls the head too slowly would not see fast blocks; that must fail
// loudly, never silently run at 3s.
func TestPacemakerSlowHeadPollIsIntegrityFailure(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[2].set(func(n *fakeNode) { n.headPoll = 60_000 })
	h.start()
	time.Sleep(400 * time.Millisecond)
	failures := h.halt()
	if !containsAny(failures, "polls the Hive head every") || !h.pm.safe.Load() {
		t.Fatalf("want an integrity failure for a 60s head poll, got %v", failures)
	}
	if e := h.pm.early.Load(); e != 0 {
		t.Fatalf("got %d early blocks with a node that cannot see them", e)
	}
}

// The same for a node that re-checks its stored blocks too slowly: every block
// would wait on it, so fast mode must refuse rather than quietly run at its pace.
func TestPacemakerSlowBlockPollIsIntegrityFailure(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[1].set(func(n *fakeNode) { n.blkPoll = 1000 })
	h.start()
	time.Sleep(400 * time.Millisecond)
	failures := h.halt()
	if !containsAny(failures, "polls stored blocks every") || !h.pm.safe.Load() {
		t.Fatalf("want an integrity failure for a 1s stored-block poll, got %v", failures)
	}
	if e := h.pm.early.Load(); e != 0 {
		t.Fatalf("got %d early blocks with a misconfigured node", e)
	}
}

// And for a node whose stream loop still sleeps 100ms-1s between checks.
func TestPacemakerSlowStreamLoopIsIntegrityFailure(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[0].set(func(n *fakeNode) { n.strSleep = 1000 })
	h.start()
	time.Sleep(400 * time.Millisecond)
	failures := h.halt()
	if !containsAny(failures, "stream loop sleeps") || !h.pm.safe.Load() {
		t.Fatalf("want an integrity failure for a 1s stream-loop sleep, got %v", failures)
	}
	if e := h.pm.early.Load(); e != 0 {
		t.Fatalf("got %d early blocks with a misconfigured node", e)
	}
}

func TestPacemakerNeverFasterThanMinGap(t *testing.T) {
	h := newPMHarness(t, 2)
	h.start()
	time.Sleep(500 * time.Millisecond)
	h.halt()
	gens := h.hived.gens()
	if len(gens) < 5 {
		t.Fatalf("too few blocks to measure: %d", len(gens))
	}
	for i := 1; i < len(gens); i++ {
		// The gap is measured from when a generate call returned to when the next
		// arrived, so arrivals can only be closer by the call's own latency.
		if gap := gens[i].Sub(gens[i-1]); gap < pmTestMinGap-3*time.Millisecond {
			t.Fatalf("blocks %d and %d only %s apart, min gap is %s", i-1, i, gap, pmTestMinGap)
		}
	}
}

// Every way a node can be busy must hold the chain to real cadence.
func TestPacemakerBusyNodeHoldsRealCadence(t *testing.T) {
	cases := map[string]func(*fakeNode){
		"behind":         func(n *fakeNode) { n.lag = 2 }, // one past the streamer's normal lag
		"peer missing":   func(n *fakeNode) { n.peers-- },
		"async tick":     func(n *fakeNode) { n.async = 1 },
		"tss ceremony":   func(n *fakeNode) { n.ceremony = 1 },
		"tss signing":    func(n *fakeNode) { n.signing = 1 },
		"tss background": func(n *fakeNode) { n.bg = 1 },
		"block producer": func(n *fakeNode) { n.bp = true },
		"election":       func(n *fakeNode) { n.ep = true },
		"gateway round":  func(n *fakeNode) { n.gw = 1 },
		"chain relay":    func(n *fakeNode) { n.orc = 1 },
	}
	for name, busy := range cases {
		name, busy := name, busy
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPMHarness(t, 3)
			h.nodes[1].set(busy)
			h.start()
			time.Sleep(700 * time.Millisecond)
			failures := h.halt()
			if len(failures) != 0 {
				t.Fatalf("a busy node is not an integrity failure, got %v", failures)
			}
			if early := h.pm.early.Load(); early != 0 {
				t.Fatalf("one node busy (%s): want no early block, got %d", name, early)
			}
			gens := h.hived.gens()
			if len(gens) < 2 {
				t.Fatalf("want capped blocks to keep the chain alive, got %d", len(gens))
			}
			for i := 1; i < len(gens); i++ {
				if gap := gens[i].Sub(gens[i-1]); gap < pmTestMaxGap-5*time.Millisecond {
					t.Fatalf("busy node: blocks only %s apart, real cadence is %s", gap, pmTestMaxGap)
				}
			}
		})
	}
}

func TestPacemakerDownNodeHoldsRealCadence(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[2].set(func(n *fakeNode) { n.down = true })
	h.start()
	time.Sleep(600 * time.Millisecond)
	failures := h.halt()
	if len(failures) != 0 || h.pm.safe.Load() {
		t.Fatalf("a stopped node is legitimate in tests, not an integrity failure: %v", failures)
	}
	if early := h.pm.early.Load(); early != 0 {
		t.Fatalf("node down: want real cadence only, got %d early blocks", early)
	}
	if h.pm.capped.Load() == 0 {
		t.Fatal("node down: the chain must keep moving at real cadence")
	}
}

func TestPacemakerResumesWhenNodeComesBack(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[0].set(func(n *fakeNode) { n.down = true })
	h.start()
	time.Sleep(400 * time.Millisecond)
	if e := h.pm.early.Load(); e != 0 {
		t.Fatalf("node down: got %d early blocks", e)
	}
	h.nodes[0].set(func(n *fakeNode) { n.down = false })
	time.Sleep(400 * time.Millisecond)
	failures := h.halt()
	if len(failures) != 0 {
		t.Fatalf("unexpected failures: %v", failures)
	}
	if e := h.pm.early.Load(); e < 5 {
		t.Fatalf("node back and idle: want early blocks to resume, got %d", e)
	}
}

func TestPacemakerResumesWhenBusyEnds(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[1].set(func(n *fakeNode) { n.ceremony = 1 })
	h.start()
	time.Sleep(400 * time.Millisecond)
	if e := h.pm.early.Load(); e != 0 {
		t.Fatalf("ceremony in flight: got %d early blocks", e)
	}
	h.nodes[1].set(func(n *fakeNode) { n.ceremony = 0 })
	time.Sleep(400 * time.Millisecond)
	h.halt()
	if e := h.pm.early.Load(); e < 5 {
		t.Fatalf("ceremony over: want early blocks to resume, got %d", e)
	}
}

// A probe that answers but cannot be trusted must never be read as idle.
func TestPacemakerProbeProtocolErrorsAreIntegrityFailures(t *testing.T) {
	cases := map[string]func(*fakeNode){
		"route missing (404)": func(n *fakeNode) { n.status = http.StatusNotFound },
		"server error":        func(n *fakeNode) { n.status = http.StatusInternalServerError },
		"not json":            func(n *fakeNode) { n.raw = "<html>sandbox</html>" },
		"field missing": func(n *fakeNode) {
			n.raw = `{"processed":999999,"async_ticks":0,"tss_ceremony":0,"tss_signing":0,"bp_busy":false,"ep_busy":false}`
		},
		"empty object": func(n *fakeNode) { n.raw = `{}` },
	}
	// Every field the pacemaker reads is required on its own: a node built
	// without any one of them must not be read as idle on that count.
	fields := reflect.TypeOf(nodeQuiescence{})
	for i := 0; i < fields.NumField(); i++ {
		tag := fields.Field(i).Tag.Get("json")
		cases["missing "+tag] = func(n *fakeNode) { n.omit = tag }
	}
	for name, broken := range cases {
		name, broken := name, broken
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newPMHarness(t, 3)
			h.nodes[1].set(broken)
			h.start()
			time.Sleep(500 * time.Millisecond)
			failures := h.halt()
			if !containsAny(failures, "probe answered but is unusable") {
				t.Fatalf("%s: want an integrity failure, got %v", name, failures)
			}
			if !h.pm.safe.Load() {
				t.Fatalf("%s: want safe mode", name)
			}
			if h.recordedCount() == 0 {
				t.Fatalf("%s: failure was not recorded", name)
			}
			if e := h.pm.early.Load(); e != 0 {
				t.Fatalf("%s: an untrusted probe must never allow an early block, got %d", name, e)
			}
		})
	}
}

func TestPacemakerGenerateFailuresEnterSafeMode(t *testing.T) {
	h := newPMHarness(t, 2)
	h.hived.set(func(f *fakeHived) { f.genErr = true })
	h.start()
	time.Sleep(800 * time.Millisecond)
	failures := h.halt()
	if !containsAny(failures, fmt.Sprintf("failed %d times in a row", pmMaxGenFailures)) || !h.pm.safe.Load() {
		t.Fatalf("want safe mode after %d generation failures, got %v", pmMaxGenFailures, failures)
	}
}

func TestPacemakerNeverRewritesChainState(t *testing.T) {
	h := newPMHarness(t, 2)
	h.start()
	time.Sleep(300 * time.Millisecond)
	h.halt()
	var edited bool
	h.hived.set(func(f *fakeHived) { edited = f.editTrueSeen })
	if edited {
		t.Fatal("pacemaker sent edit_if_needed=true: it may rewrite witness signing keys")
	}
	if len(h.hived.gens()) == 0 {
		t.Fatal("no blocks produced")
	}
}

func TestPacemakerWitnessKeyChangeIsIntegrityFailure(t *testing.T) {
	h := newPMHarness(t, 2)
	h.start()
	h.waitActivated()
	time.Sleep(100 * time.Millisecond)
	h.hived.set(func(f *fakeHived) { f.witnessKey = "STM-someone-elses-key" })
	failures := h.halt()
	if !containsAny(failures, "signing key changed") {
		t.Fatalf("want a witness-key integrity failure, got %v", failures)
	}
}

func TestPacemakerNeverTookOverIsIntegrityFailure(t *testing.T) {
	h := newPMHarness(t, 3)
	for _, n := range h.nodes {
		n.set(func(n *fakeNode) { n.down = true })
	}
	h.start()
	time.Sleep(400 * time.Millisecond)
	failures := h.halt()
	if !containsAny(failures, "never took over") {
		t.Fatalf("want a never-took-over integrity failure, got %v", failures)
	}
	if n := len(h.hived.gens()); n != 0 {
		t.Fatalf("before takeover block production is the witness's job, pacemaker produced %d", n)
	}
}

// A block produced by someone else (the witness, during takeover) restarts the
// real-cadence clock, so it can never be followed by a fast block while busy.
func TestPacemakerExternalBlockRestartsCadence(t *testing.T) {
	h := newPMHarness(t, 2)
	h.nodes[0].set(func(n *fakeNode) { n.ceremony = 1 })
	h.start()
	h.waitActivated()
	// Land the external block halfway through a pacemaker interval: without the
	// reset the pacemaker's next block would follow it after only half an interval.
	// Halfway is also far from any moment the pacemaker produces, so no race.
	n0 := len(h.hived.gens())
	for len(h.hived.gens()) == n0 {
		time.Sleep(2 * time.Millisecond)
	}
	time.Sleep(pmTestMaxGap / 2)
	before := len(h.hived.gens())
	external := time.Now()
	h.hived.set(func(f *fakeHived) { f.head++ })
	deadline := time.Now().Add(2 * time.Second)
	for len(h.hived.gens()) == before && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	h.halt()
	gens := h.hived.gens()
	if len(gens) == before {
		t.Fatal("no pacemaker block after the external one")
	}
	if gap := gens[before].Sub(external); gap < pmTestMaxGap-5*time.Millisecond {
		t.Fatalf("pacemaker block %s after an external block, real cadence is %s", gap, pmTestMaxGap)
	}
}

// A network fault the harness has in effect holds real cadence, and fast blocks
// resume once it is cleared.
func TestPacemakerHarnessFaultHoldsRealCadence(t *testing.T) {
	h := newPMHarness(t, 3)
	h.setHeld("magi-2:iptables filter/INPUT")
	h.start()
	time.Sleep(700 * time.Millisecond)
	if early := h.pm.early.Load(); early != 0 {
		t.Fatalf("fault in effect: want no early block, got %d", early)
	}
	gens := h.hived.gens()
	if len(gens) < 2 {
		t.Fatalf("want capped blocks to keep the chain alive, got %d", len(gens))
	}
	for i := 1; i < len(gens); i++ {
		if gap := gens[i].Sub(gens[i-1]); gap < pmTestMaxGap-5*time.Millisecond {
			t.Fatalf("fault in effect: blocks only %s apart, real cadence is %s", gap, pmTestMaxGap)
		}
	}
	h.setHeld("")
	time.Sleep(300 * time.Millisecond)
	failures := h.halt()
	if len(failures) != 0 {
		t.Fatalf("a harness fault is not an integrity failure, got %v", failures)
	}
	if early := h.pm.early.Load(); early == 0 {
		t.Fatalf("fault cleared: want early blocks again, got none")
	}
}

// Until armed (boot), blocks come at real cadence even with every node idle;
// arming lets them come early.
func TestPacemakerBootRunsAtRealCadenceUntilArmed(t *testing.T) {
	h := newPMHarness(t, 3)
	h.unarmed = true
	h.start()
	time.Sleep(500 * time.Millisecond)
	if early := h.pm.early.Load(); early != 0 {
		t.Fatalf("not armed: want no early block, got %d", early)
	}
	gens := h.hived.gens()
	if len(gens) < 2 {
		t.Fatalf("want capped blocks during boot, got %d", len(gens))
	}
	for i := 1; i < len(gens); i++ {
		if gap := gens[i].Sub(gens[i-1]); gap < pmTestMaxGap-5*time.Millisecond {
			t.Fatalf("not armed: blocks only %s apart, real cadence is %s", gap, pmTestMaxGap)
		}
	}
	h.pm.arm()
	time.Sleep(300 * time.Millisecond)
	failures := h.halt()
	if len(failures) != 0 {
		t.Fatalf("boot is not an integrity failure, got %v", failures)
	}
	if early := h.pm.early.Load(); early == 0 {
		t.Fatal("armed: want early blocks, got none")
	}
}

// Nodes that disagree on gossip (a transaction one has stored and another has
// not, or readiness attestations for a target still ahead) keep the chain at
// real cadence; a difference only at or below head does not.
func TestPacemakerHoldsRealCadenceUntilGossipConverges(t *testing.T) {
	cases := []struct {
		name      string
		diverge   func(n *fakeNode)
		wantEarly bool
	}{
		{"converged", func(n *fakeNode) {}, true},
		{"mempool diverged", func(n *fakeNode) { n.txSeen = 7 }, false},
		{"attestations diverged ahead of head", func(n *fakeNode) {
			n.atts = map[string][]string{"999999": {"magi.test1"}}
		}, false},
		{"attestations differ only below head", func(n *fakeNode) {
			n.atts = map[string][]string{"999999": {"magi.test1", "magi.test2"}, "3": {"magi.test1"}}
		}, true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newPMHarness(t, 3)
			for _, n := range h.nodes {
				n.set(func(n *fakeNode) {
					n.txSeen = 6
					n.atts = map[string][]string{"999999": {"magi.test1", "magi.test2"}}
				})
			}
			h.nodes[1].set(c.diverge)
			h.start()
			time.Sleep(500 * time.Millisecond)
			failures := h.halt()
			if len(failures) != 0 {
				t.Fatalf("gossip divergence is not an integrity failure, got %v", failures)
			}
			if early := h.pm.early.Load(); (early > 0) != c.wantEarly {
				t.Fatalf("%s: early blocks = %d, want early=%v", c.name, early, c.wantEarly)
			}
		})
	}
}

// A node that cannot count its transactions must not make the mempool check pass
// by agreeing on "unknown" with every other node.
func TestPacemakerUnknownTxCountIsIntegrityFailure(t *testing.T) {
	h := newPMHarness(t, 3)
	for _, n := range h.nodes {
		n.set(func(n *fakeNode) { n.txSeen = -1 })
	}
	h.start()
	time.Sleep(400 * time.Millisecond)
	failures := h.halt()
	if !containsAny(failures, "cannot count its stored transactions") || !h.pm.safe.Load() {
		t.Fatalf("want an integrity failure for tx_seen=-1, got %v", failures)
	}
	if e := h.pm.early.Load(); e != 0 {
		t.Fatalf("got %d early blocks with no mempool view", e)
	}
}

// While the chain is held, nodes are probed at pmHeldProbeInterval, not every
// 15ms: the probes cost the busy nodes CPU (FD-20).
func TestPacemakerProbesLessOftenWhileHeld(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[1].set(func(n *fakeNode) { n.ceremony = 1 })
	h.start()
	h.waitActivated()
	h.nodes[0].set(func(n *fakeNode) { n.hits = 0 })
	const window = 600 * time.Millisecond
	time.Sleep(window)
	h.halt()
	var hits int
	h.nodes[0].set(func(n *fakeNode) { hits = n.hits })
	// Probes come at the held interval, plus one round per capped block.
	if most := int(window/pmHeldProbeInterval) + int(window/pmTestMaxGap) + 2; hits > most {
		t.Fatalf("%d probes of a node in %s while held, want at most %d", hits, window, most)
	}
}

// A hold that ends with a wake signal (the test began waiting) releases the next
// fast block at once, not at the next held probe. The release comes right after
// a probe, so without the wake the next block would wait a whole held interval.
func TestPacemakerWakeEndsAHoldAtOnce(t *testing.T) {
	const heldProbe = 400 * time.Millisecond
	h := newPMHarness(t, 3)
	h.wake = make(chan struct{}, 1)
	h.maxGap = 5 * time.Second // no capped block in the way
	h.heldProbe = heldProbe
	h.setHeld("test running")
	h.start()
	h.waitActivated()
	hits := func() (n int) { h.nodes[0].set(func(f *fakeNode) { n = f.hits }); return }
	for start := hits(); hits() == start; time.Sleep(time.Millisecond) {
	}
	before := len(h.hived.gens())
	released := time.Now()
	h.setHeld("")
	h.wake <- struct{}{}
	var first time.Duration
	for deadline := time.Now().Add(2 * heldProbe); time.Now().Before(deadline); time.Sleep(2 * time.Millisecond) {
		if gens := h.hived.gens(); len(gens) > before {
			first = gens[before].Sub(released)
			break
		}
	}
	h.halt()
	if first == 0 || first > heldProbe/4 {
		t.Fatalf("first block %s after the wake (0: none), want well under the %s held probe interval", first, heldProbe)
	}
}

// The block log has one line per produced block: early ones while every node is
// idle, capped ones naming what held the chain.
func TestPacemakerBlockLogRecordsEveryBlock(t *testing.T) {
	var buf bytes.Buffer
	h := newPMHarness(t, 3)
	h.blockLog = &buf
	h.start()
	time.Sleep(200 * time.Millisecond)
	h.setHeld("test running")
	time.Sleep(2*pmTestMaxGap + 50*time.Millisecond)
	h.halt()
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	if len(lines) != len(h.hived.gens()) {
		t.Fatalf("%d block log lines for %d generated blocks", len(lines), len(h.hived.gens()))
	}
	var early, held int
	for _, l := range lines {
		f := strings.Split(l, "\t")
		if len(f) != 6 {
			t.Fatalf("block log line %q: want 6 tab-separated fields", l)
		}
		switch {
		case f[2] == "early" && f[4] == "":
			early++
		case f[2] == "capped" && strings.Contains(f[4], "harness{test running}"):
			held++
		}
	}
	if early == 0 || held < 2 {
		t.Fatalf("want early lines while idle and capped lines naming the hold, got %d early, %d held:\n%s", early, held, buf.String())
	}
}

// A node that persists its last processed block once a second would have every
// harness wait on it act up to ten fast blocks late; that must fail loudly.
func TestPacemakerSlowCheckpointIsIntegrityFailure(t *testing.T) {
	h := newPMHarness(t, 3)
	h.nodes[2].set(func(n *fakeNode) { n.checkpoint = 1000 })
	h.start()
	time.Sleep(400 * time.Millisecond)
	failures := h.halt()
	if !containsAny(failures, "VSC_DEVNET_CHECKPOINT") || !h.pm.safe.Load() {
		t.Fatalf("want an integrity failure for a 1s checkpoint, got %v", failures)
	}
	if e := h.pm.early.Load(); e != 0 {
		t.Fatalf("safe mode must not produce early blocks, got %d", e)
	}
}
