package devnet

// Harness-driven Hive block production for Config.FastBlocks.
//
// The 3s Hive block is the floor under every devnet test, yet for most of those
// seconds no node is doing anything. With FastBlocks the harness produces each
// block itself (hived debug_node_api.debug_generate_blocks) as soon as every magi
// node reports that it has processed the head and finished the work that block
// triggered (GET /devnet/quiescence), and never more than one Hive interval after
// the previous block, so the chain is never slower than real time. A running TSS
// ceremony keeps a node busy, so ceremonies proceed at real cadence and span the
// same number of blocks they would on a live chain.
//
// Until a node answers its quiescence probe the witness keeps producing. From
// then on the pacemaker drives the chain, but only at real cadence until the
// harness arms it once the genesis node is restarted: boot up to there waits on
// wall clock (node start-up, witness registration, the genesis election), so early
// blocks would move the genesis height.
// The first early block puts chain time ahead of wall clock; from then on the
// witness's next slot is always in the future and the pacemaker alone drives
// the chain.
//
// Integrity contract: the pacemaker may make a test faster, never different.
//   - An early block needs EVERY configured node to answer idle. A node that is
//     down or restarting forces real cadence, so a test that stops a node for N
//     seconds sees the real number of blocks pass meanwhile. So does a node not
//     yet connected to every other node (still dialing after a restart), and any
//     network fault the harness has in effect (partition, latency): connection
//     and timeout dynamics run on wall clock, so they get real-cadence blocks.
//   - edit_if_needed is false: generation can never rewrite chain state (witness
//     signing keys). If it would have to, the call fails instead.
//   - Any anomaly (a probe protocol error, repeated generation failures, a witness
//     signing key that changed, never taking over) puts the pacemaker in safe mode
//     (real cadence) for the rest of the run and is recorded as an integrity
//     failure. Stop returns it as an *IntegrityError, the start helpers fail the
//     test on it, and the package TestMain fails the run if any was recorded, so
//     none of it can pass silently.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// hiveBlockInterval is Hive's block interval: the pacemaker never waits longer.
const hiveBlockInterval = 3 * time.Second

const defaultFastBlockMinInterval = 100 * time.Millisecond

// pmHeldProbeInterval is how often nodes are probed while the chain is held. Five
// nodes probed every 15ms (each probe also counts stored transactions) is load on
// the box while it does the work being waited for. A hold ends at the cap or when
// the work ends; 100ms later at most is well inside a 3s block.
const pmHeldProbeInterval = 100 * time.Millisecond

// pmMaxGenFailures consecutive generation failures put the pacemaker in safe mode.
const pmMaxGenFailures = 3

// pmWitnessCheckEvery is how often (in pacemaker blocks) the witness signing keys
// are re-checked against the snapshot taken at takeover.
const pmWitnessCheckEvery = 100

// streamerHeadLag is how far behind the Hive head a caught-up node is by design:
// the streamer fetches [cursor, head-1] (fetchBlockBatch(start, head-start)) and
// treats cursor == head as caught up, so block N is processed once N+1 exists.
// A live chain is no different: at real cadence nodes process each block 2.9 to
// 4.3s after it is produced (median about 3s; TestFastBlocksTrace READLAG, five
// nodes, 190 blocks), that is, about when the next block appears.
const streamerHeadLag = 1

// pmMaxHeadPoll is the slowest head polling a node may have under FastBlocks. A
// node polling slower would not see blocks produced faster than it polls.
const pmMaxHeadPoll = time.Second

// pmMaxBlockPoll is the slowest stored-block polling a node may have under
// FastBlocks: a node re-checks Mongo for newly stored blocks this often, so each
// block waits up to this long before the node processes it (VSC_DEVNET_BLOCK_POLL).
const pmMaxBlockPoll = 250 * time.Millisecond

// nodeQuiescence mirrors the node's GET /devnet/quiescence response. Every field
// is required: a probe from a build that lacks one is a protocol error, never
// silently read as idle.
type nodeQuiescence struct {
	HeadPollMs    *int64  `json:"head_poll_ms"`
	BlockPollMs   *int64  `json:"block_poll_ms"`
	StreamSleepMs *int64  `json:"stream_sleep_ms"`
	CheckpointMs  *int64  `json:"checkpoint_ms"`
	Peers         *int    `json:"peers"`
	Processed     *uint64 `json:"processed"`
	AsyncTicks    *int64  `json:"async_ticks"`
	TssCeremony   *int    `json:"tss_ceremony"`
	TssSigning    *int    `json:"tss_signing"`
	TssBackground *int64  `json:"tss_background"`
	BpBusy        *bool   `json:"bp_busy"`
	EpBusy        *bool   `json:"ep_busy"`
	GatewayBg     *int64  `json:"gateway_bg"`
	OracleBg      *int64  `json:"oracle_bg"`
	// Gossip state compared across nodes (gossipDiverged): transactions stored,
	// readiness attestations held per target.
	TxSeen       *int64               `json:"tx_seen"`
	Attestations *map[string][]string `json:"tss_attestations"`
}

func (q nodeQuiescence) complete() bool {
	return q.HeadPollMs != nil && q.BlockPollMs != nil && q.StreamSleepMs != nil && q.CheckpointMs != nil && q.Peers != nil && q.Processed != nil && q.AsyncTicks != nil && q.TssCeremony != nil &&
		q.TssSigning != nil && q.TssBackground != nil && q.BpBusy != nil && q.EpBusy != nil &&
		q.GatewayBg != nil && q.OracleBg != nil && q.TxSeen != nil && q.Attestations != nil
}

// gossipDiverged names the gossip that has not yet reached every node ("" when
// converged): a transaction one node stored and another has not, or a readiness
// target above head whose attestations differ between nodes. Probes are timed
// by nothing but block processing, so without this check a readiness window or
// a transaction's path to the next L2 slot could close faster on fast blocks than
// gossip crosses the network; at real cadence it has seconds. Targets at or
// below head are left out: nodes up to a block apart evict them at different
// moments, and their sessions have already started.
func gossipDiverged(reports []nodeQuiescence, head uint64) string {
	var why []string
	tx := make(map[int64]bool)
	for _, q := range reports {
		tx[*q.TxSeen] = true
	}
	if len(tx) > 1 {
		counts := make([]string, len(reports))
		for i, q := range reports {
			counts[i] = strconv.FormatInt(*q.TxSeen, 10)
		}
		why = append(why, "mempool{"+strings.Join(counts, "/")+"}")
	}
	targets := make(map[string]bool)
	for _, q := range reports {
		for t := range *q.Attestations {
			if n, err := strconv.ParseUint(t, 10, 64); err == nil && n > head {
				targets[t] = true
			}
		}
	}
	keys := make([]string, 0, len(targets))
	for t := range targets {
		keys = append(keys, t)
	}
	sort.Strings(keys)
	for _, t := range keys {
		views := make(map[string]bool)
		for _, q := range reports {
			views[strings.Join((*q.Attestations)[t], ",")] = true
		}
		if len(views) > 1 {
			why = append(why, "gossip{"+t+"}")
		}
	}
	return strings.Join(why, " ")
}

// busyFields names what keeps the node from being idle at head with wantPeers
// connected peers ("" when idle).
func (q nodeQuiescence) busyFields(head uint64, wantPeers int) string {
	var f []string
	if *q.Peers < wantPeers {
		f = append(f, fmt.Sprintf("peers:%d/%d", *q.Peers, wantPeers))
	}
	if *q.Processed+streamerHeadLag < head {
		f = append(f, fmt.Sprintf("behind:%d", head-*q.Processed))
	}
	if *q.AsyncTicks != 0 {
		f = append(f, fmt.Sprintf("async:%d", *q.AsyncTicks))
	}
	if *q.TssCeremony != 0 {
		f = append(f, fmt.Sprintf("ceremony:%d", *q.TssCeremony))
	}
	if *q.TssSigning != 0 {
		f = append(f, fmt.Sprintf("signing:%d", *q.TssSigning))
	}
	if *q.TssBackground != 0 {
		f = append(f, fmt.Sprintf("tss-bg:%d", *q.TssBackground))
	}
	if *q.BpBusy {
		f = append(f, "block-producer")
	}
	if *q.EpBusy {
		f = append(f, "election")
	}
	if *q.GatewayBg != 0 {
		f = append(f, fmt.Sprintf("gateway:%d", *q.GatewayBg))
	}
	if *q.OracleBg != 0 {
		f = append(f, fmt.Sprintf("chain-relay:%d", *q.OracleBg))
	}
	return strings.Join(f, ",")
}

// probeProtocolError is a probe that answered but not with a valid quiescence
// report (missing route, wrong status, bad or incomplete body): the node is up but
// cannot be gated on, which is never safe to ignore.
type probeProtocolError struct{ error }

// pacemakerConfig is everything the pacemaker depends on, so it can be driven
// against fakes in unit tests.
type pacemakerConfig struct {
	rpc      string   // hived JSON-RPC
	probes   []string // one GET /devnet/quiescence URL per configured node
	debugKey string   // witness WIF that signs generated blocks
	minGap   time.Duration
	maxGap   time.Duration
	// held names a fault the harness has in effect, or the test running its
	// own code ("" when none); while it is non-empty every block comes at real
	// cadence.
	held func() string
	// wake, if set, ends a pause early (a test wait began; see testWaitStarted).
	wake <-chan struct{}
	// heldProbe is how often nodes are probed while the chain is held
	// (default pmHeldProbeInterval).
	heldProbe time.Duration
	// blockLog, if set, gets one tab-separated line per produced block: wall
	// time, height, early or capped, the lowest height every node had processed,
	// what held the chain, and (waiters) whose test-clock waits were in progress.
	blockLog io.Writer
	waiters  func() string
	// lead is the test clock's lead over wall clock (devclock.go), raised to
	// the chain time produced since takeover less the wall time it took.
	lead *atomic.Int64
	// record receives each integrity failure (the package registry for real
	// devnets, a local sink in unit tests).
	record func(string)
}

type pacemaker struct {
	cfg    pacemakerConfig
	client *http.Client
	stop   context.CancelFunc
	done   chan struct{}

	early     atomic.Int64
	capped    atomic.Int64
	activated atomic.Bool
	armed     atomic.Bool // early blocks allowed (arm); boot runs at real cadence
	safe      atomic.Bool

	mu          sync.Mutex
	failures    []string
	witnessKeys map[string]string
}

func newPacemaker(cfg pacemakerConfig) *pacemaker {
	if cfg.minGap <= 0 {
		cfg.minGap = defaultFastBlockMinInterval
	}
	if cfg.maxGap <= 0 {
		cfg.maxGap = hiveBlockInterval
	}
	if cfg.heldProbe <= 0 {
		cfg.heldProbe = pmHeldProbeInterval
	}
	return &pacemaker{
		cfg:    cfg,
		client: &http.Client{Timeout: 500 * time.Millisecond},
		done:   make(chan struct{}),
	}
}

// fail records an integrity failure and puts the pacemaker in safe mode.
func (p *pacemaker) fail(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	p.mu.Lock()
	p.failures = append(p.failures, msg)
	p.mu.Unlock()
	if p.cfg.record != nil {
		p.cfg.record("pacemaker: " + msg)
	}
	if !p.safe.Swap(true) {
		log.Printf("[pacemaker] INTEGRITY FAILURE, safe mode (real cadence) from here on: %s", msg)
	} else {
		log.Printf("[pacemaker] INTEGRITY FAILURE: %s", msg)
	}
}

// arm allows early blocks from now on. Until then every block comes at real
// cadence, whatever the nodes report.
func (p *pacemaker) arm() {
	if !p.armed.Swap(true) {
		log.Printf("[pacemaker] armed: fast blocks from here on")
	}
}

// start runs the pacemaker loop in the background until halt.
func (p *pacemaker) start() {
	ctx, cancel := context.WithCancel(context.Background())
	p.stop = cancel
	go p.run(ctx)
}

func (p *pacemaker) run(ctx context.Context) {
	defer close(p.done)
	pause := func(t time.Duration) bool {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(t):
			return true
		case <-p.cfg.wake:
			return true
		}
	}
	var last, lastDiag time.Time
	var lastHead uint64
	genFails := 0
	// Test clock: each block is 3s of chain time. A block that came sooner than
	// that moves the test clock's lead forward by the difference, so every fast
	// block advances the test clock by one whole block. A block that took 3s or
	// longer (held at the cap, or after a stall) is wall clock already and moves
	// nothing. Measuring the lead as chain time since takeover less wall time
	// instead (v1-v8) let every stall and every slow capped block build a
	// deficit that the next fast blocks repaid without moving the test clock, so
	// a wait right after such a stretch spanned more blocks than its duration
	// (FD-23, TestPacemakerLeadAfterALongHold).
	var lastBlockWall time.Time
	advanceLead := func() {
		now := time.Now()
		if p.cfg.lead != nil && p.activated.Load() && !lastBlockWall.IsZero() {
			if gain := leadGain(now.Sub(lastBlockWall)); gain > 0 {
				p.cfg.lead.Add(int64(gain))
			}
		}
		lastBlockWall = now
	}
	probeBroken := make(map[int]bool)
	for {
		head, err := pmHeadBlock(ctx, p.client, p.cfg.rpc)
		if err != nil {
			if !pause(100 * time.Millisecond) {
				return
			}
			continue
		}
		// Cadence counts from the latest block whoever produced it, so the
		// witness's last block during takeover never yields a fast interval.
		if head > lastHead {
			lastHead = head
			last = time.Now()
			advanceLead()
		}

		reachable, idle := 0, true
		var blockers []string
		var reports []nodeQuiescence
		for i, url := range p.cfg.probes {
			n := i + 1
			q, err := pmQuiescence(ctx, p.client, url)
			var perr probeProtocolError
			switch {
			case err == nil:
				reachable++
				reports = append(reports, q)
				if busy := q.busyFields(head, len(p.cfg.probes)-1); busy != "" {
					idle = false
					blockers = append(blockers, fmt.Sprintf("magi-%d{%s}", n, busy))
				}
			case errors.As(err, &perr):
				idle = false
				blockers = append(blockers, fmt.Sprintf("magi-%d{probe-error}", n))
				if !probeBroken[n] {
					probeBroken[n] = true
					p.fail("magi-%d quiescence probe answered but is unusable: %v", n, err)
				}
			default:
				idle = false // down or restarting: real cadence until it is back
				blockers = append(blockers, fmt.Sprintf("magi-%d{down}", n))
			}
		}
		if reachable == len(p.cfg.probes) {
			if why := gossipDiverged(reports, head); why != "" {
				idle = false
				blockers = append(blockers, why)
			}
		}
		if p.cfg.held != nil {
			if why := p.cfg.held(); why != "" {
				idle = false
				blockers = append(blockers, "harness{"+why+"}")
			}
		}

		if !p.activated.Load() {
			if reachable == 0 {
				if !pause(200 * time.Millisecond) {
					return
				}
				continue
			}
			keys, err := pmWitnessKeys(ctx, p.client, p.cfg.rpc)
			if err != nil {
				p.fail("cannot snapshot witness signing keys at takeover: %v", err)
			}
			p.mu.Lock()
			p.witnessKeys = keys
			p.mu.Unlock()
			lastBlockWall = time.Now()
			p.activated.Store(true)
			log.Printf("[pacemaker] taking over Hive block production at head %d (min gap %s, max gap %s, %d witness keys pinned)",
				head, p.cfg.minGap, p.cfg.maxGap, len(keys))
		}

		since := time.Since(last)
		early := !p.safe.Load() && p.armed.Load() && reachable == len(p.cfg.probes) && idle && since >= p.cfg.minGap
		if !early && since < p.cfg.maxGap {
			wait := 15 * time.Millisecond
			if !idle {
				wait = p.cfg.heldProbe
			}
			if rest := p.cfg.maxGap - since; wait > rest {
				wait = rest
			}
			if !pause(wait) {
				return
			}
			continue
		}
		if err := pmGenerateBlock(ctx, p.client, p.cfg.rpc, p.cfg.debugKey); err != nil {
			if ctx.Err() != nil {
				return
			}
			genFails++
			log.Printf("[pacemaker] debug_generate_blocks failed (%d in a row): %v", genFails, err)
			if genFails == pmMaxGenFailures {
				p.fail("debug_generate_blocks failed %d times in a row: %v", genFails, err)
			}
			if !pause(100 * time.Millisecond) {
				return
			}
			continue
		}
		genFails = 0
		lastHead = head + 1
		last = time.Now()
		advanceLead()
		if w := p.cfg.blockLog; w != nil {
			kind, minProcessed := "capped", int64(-1)
			if early {
				kind = "early"
			}
			for _, q := range reports {
				if minProcessed < 0 || int64(*q.Processed) < minProcessed {
					minProcessed = int64(*q.Processed)
				}
			}
			waiting := ""
			if p.cfg.waiters != nil {
				waiting = p.cfg.waiters()
			}
			fmt.Fprintf(w, "%s\t%d\t%s\t%d\t%s\t%s\n", last.Format("15:04:05.000"), head+1, kind, minProcessed, strings.Join(blockers, " "), waiting)
		}
		if early {
			p.early.Add(1)
		} else {
			p.capped.Add(1)
			// Say what held the chain to real cadence, at most every 10s, so a run
			// that never speeds up explains itself.
			if time.Since(lastDiag) >= 10*time.Second {
				lastDiag = time.Now()
				why := strings.Join(blockers, " ")
				if p.safe.Load() {
					why = "safe mode"
				} else if !p.armed.Load() {
					why = "boot (not armed yet)"
				}
				log.Printf("[pacemaker] block %d at the %s cap (early %d / capped %d), held by: %s",
					head+1, p.cfg.maxGap, p.early.Load(), p.capped.Load(), why)
			}
		}
		if total := p.early.Load() + p.capped.Load(); total%pmWitnessCheckEvery == 0 {
			p.checkWitnessKeys(ctx)
		}
	}
}

// checkWitnessKeys compares every witness signing key with the takeover snapshot.
func (p *pacemaker) checkWitnessKeys(ctx context.Context) {
	now, err := pmWitnessKeys(ctx, p.client, p.cfg.rpc)
	if err != nil {
		if ctx.Err() == nil {
			p.fail("cannot re-read witness signing keys: %v", err)
		}
		return
	}
	p.mu.Lock()
	pinned := p.witnessKeys
	p.mu.Unlock()
	for owner, key := range pinned {
		if now[owner] != key {
			p.fail("witness %s signing key changed: %s -> %s", owner, key, now[owner])
		}
	}
}

// halt stops block production and returns every integrity failure recorded. Call
// it while hived is still up.
func (p *pacemaker) halt() []string {
	p.stop()
	<-p.done
	if !p.activated.Load() {
		p.fail("never took over block production: no node answered GET /devnet/quiescence")
	} else {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		p.checkWitnessKeys(ctx)
		cancel()
	}
	log.Printf("[pacemaker] stopped: %d early blocks, %d at the %s cap, safe mode %v",
		p.early.Load(), p.capped.Load(), p.cfg.maxGap, p.safe.Load())
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.failures...)
}

// ===== Devnet wiring =====

// enableHivedDebugPlugins adds debug_node and debug_node_api to the hived plugin
// line of the config.ini that createHAFDataDirs wrote.
func enableHivedDebugPlugins(hafDataDir string) error {
	path := filepath.Join(hafDataDir, "config.ini")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	const marker = "plugin = witness "
	if !bytes.Contains(raw, []byte(marker)) {
		return fmt.Errorf("config.ini has no %q line to extend", strings.TrimSpace(marker))
	}
	raw = bytes.Replace(raw, []byte(marker), []byte(marker+"debug_node debug_node_api "), 1)
	return os.WriteFile(path, raw, 0o644)
}

func (d *Devnet) quiescenceURL(node int) string {
	return strings.Replace(d.GQLEndpoint(node), "/api/v1/graphql", "/devnet/quiescence", 1)
}

// PacemakerStats reports how many blocks the pacemaker produced early (all nodes
// quiescent), how many at the one-interval cap, and whether it fell back to safe
// mode.
func (d *Devnet) PacemakerStats() (early, capped int64, safe bool) {
	if d.pm == nil {
		return 0, 0, false
	}
	return d.pm.early.Load(), d.pm.capped.Load(), d.pm.safe.Load()
}

// startPacemaker runs until stopPacemaker. It is a no-op unless FastBlocks.
func (d *Devnet) startPacemaker() {
	if !d.cfg.FastBlocks {
		return
	}
	cfg := d.pacemakerConfig()
	if dir := os.Getenv("DEVNET_LOG_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			if f, err := os.Create(filepath.Join(dir, "pacemaker-blocks.tsv")); err == nil {
				cfg.blockLog, d.pmBlockLog = f, f
			}
		}
	}
	d.pm = newPacemaker(cfg)
	d.pm.start()
}

// pacemakerConfig wires the pacemaker to this devnet.
func (d *Devnet) pacemakerConfig() pacemakerConfig {
	probes := make([]string, d.cfg.Nodes)
	for n := 1; n <= d.cfg.Nodes; n++ {
		probes[n-1] = d.quiescenceURL(n)
	}
	project := d.projectName
	return pacemakerConfig{
		rpc:      d.HiveRPCEndpoint(),
		probes:   probes,
		debugKey: d.cfg.InitminerWIF,
		minGap:   d.cfg.FastBlockMinInterval,
		maxGap:   hiveBlockInterval,
		held:     d.harnessHold,
		wake:     testWaitStarted,
		waiters:  activeTestWaiters,
		lead:     &testClockLead,
		record:   func(msg string) { recordIntegrityFailure(project, msg) },
	}
}

// armPacemaker allows fast blocks once Start has finished booting.
func (d *Devnet) armPacemaker() {
	if d.pm != nil {
		d.pm.arm()
	}
}

// stopPacemaker stops block production and returns the integrity failures the
// pacemaker recorded. Call it while hived is still up.
func (d *Devnet) stopPacemaker() []string {
	if d.pm == nil {
		return nil
	}
	failures := d.pm.halt()
	if d.pmBlockLog != nil {
		d.pmBlockLog.Close()
	}
	return failures
}

// ===== RPC =====

func pmRPC(ctx context.Context, client *http.Client, url, body string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("decode %s: %w", strings.TrimSpace(string(raw)), err)
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return fmt.Errorf("rpc error: %s", env.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(env.Result, out)
}

func pmHeadBlock(ctx context.Context, client *http.Client, rpc string) (uint64, error) {
	var props struct {
		HeadBlockNumber uint64 `json:"head_block_number"`
	}
	err := pmRPC(ctx, client, rpc, `{"jsonrpc":"2.0","id":1,"method":"database_api.get_dynamic_global_properties"}`, &props)
	return props.HeadBlockNumber, err
}

// pmGenerateBlock produces exactly one block. edit_if_needed is false so a key
// mismatch fails the call rather than rewriting the witness in chain state.
func pmGenerateBlock(ctx context.Context, client *http.Client, rpc, debugKey string) error {
	body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"debug_node_api.debug_generate_blocks","params":{"debug_key":%q,"count":1,"skip":0,"miss_blocks":0,"edit_if_needed":false}}`, debugKey)
	var res struct {
		Blocks int `json:"blocks"`
	}
	if err := pmRPC(ctx, client, rpc, body, &res); err != nil {
		return err
	}
	if res.Blocks != 1 {
		return fmt.Errorf("generated %d blocks, want 1", res.Blocks)
	}
	return nil
}

// pmWitnessKeys returns every witness's signing key, by owner.
func pmWitnessKeys(ctx context.Context, client *http.Client, rpc string) (map[string]string, error) {
	var res struct {
		Witnesses []struct {
			Owner      string `json:"owner"`
			SigningKey string `json:"signing_key"`
		} `json:"witnesses"`
	}
	err := pmRPC(ctx, client, rpc, `{"jsonrpc":"2.0","id":1,"method":"database_api.list_witnesses","params":{"start":"","limit":1000,"order":"by_name"}}`, &res)
	if err != nil {
		return nil, err
	}
	keys := make(map[string]string, len(res.Witnesses))
	for _, w := range res.Witnesses {
		keys[w.Owner] = w.SigningKey
	}
	if len(keys) == 0 {
		return nil, errors.New("list_witnesses returned no witnesses")
	}
	return keys, nil
}

func pmQuiescence(ctx context.Context, client *http.Client, url string) (nodeQuiescence, error) {
	var q nodeQuiescence
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return q, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return q, err // down or restarting
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return q, probeProtocolError{fmt.Errorf("HTTP %d from %s", resp.StatusCode, url)}
	}
	if err := json.NewDecoder(resp.Body).Decode(&q); err != nil {
		return q, probeProtocolError{fmt.Errorf("decode %s: %w", url, err)}
	}
	if !q.complete() {
		return q, probeProtocolError{fmt.Errorf("%s is missing quiescence fields (node built without them?)", url)}
	}
	if poll := time.Duration(*q.HeadPollMs) * time.Millisecond; poll > pmMaxHeadPoll {
		return q, probeProtocolError{fmt.Errorf("%s: node polls the Hive head every %s, FastBlocks needs at most %s (VSC_DEVNET_HEAD_POLL not applied?)", url, poll, pmMaxHeadPoll)}
	}
	if *q.TxSeen < 0 {
		return q, probeProtocolError{fmt.Errorf("%s cannot count its stored transactions (tx_seen=%d)", url, *q.TxSeen)}
	}
	if sleep := time.Duration(*q.StreamSleepMs) * time.Millisecond; sleep > pmMaxBlockPoll {
		return q, probeProtocolError{fmt.Errorf("%s: node's stream loop sleeps %s when caught up, FastBlocks needs at most %s (VSC_DEVNET_STREAM_SLEEP not applied?)", url, sleep, pmMaxBlockPoll)}
	}
	if poll := time.Duration(*q.BlockPollMs) * time.Millisecond; poll > pmMaxBlockPoll {
		return q, probeProtocolError{fmt.Errorf("%s: node polls stored blocks every %s, FastBlocks needs at most %s (VSC_DEVNET_BLOCK_POLL not applied?)", url, poll, pmMaxBlockPoll)}
	}
	if every := time.Duration(*q.CheckpointMs) * time.Millisecond; every > pmMaxBlockPoll {
		return q, probeProtocolError{fmt.Errorf("%s: node persists its last processed block every %s, FastBlocks needs at most %s, or harness waits on it act blocks late (VSC_DEVNET_CHECKPOINT not applied?)", url, every, pmMaxBlockPoll)}
	}
	return q, nil
}

// ===== integrity reporting =====

// IntegrityError is returned by Stop when the fast-mode machinery (pacemaker,
// pre-params pool) recorded anything that could have made the run differ from a
// real-cadence run.
type IntegrityError struct {
	Project  string
	Failures []string
}

func (e *IntegrityError) Error() string {
	return fmt.Sprintf("devnet %s fast-mode integrity failures:\n  - %s", e.Project, strings.Join(e.Failures, "\n  - "))
}

var (
	integrityMu  sync.Mutex
	integrityLog []string
)

func recordIntegrityFailure(project, msg string) {
	integrityMu.Lock()
	defer integrityMu.Unlock()
	integrityLog = append(integrityLog, project+": "+msg)
}

// IntegrityFailures returns every integrity failure recorded in this process.
func IntegrityFailures() []string {
	integrityMu.Lock()
	defer integrityMu.Unlock()
	out := append([]string(nil), integrityLog...)
	sort.Strings(out)
	return out
}

// harnessHold names what keeps this devnet at real cadence on the harness side:
// a network fault in effect, or the test running its own code rather than
// waiting on the test clock (testWaits). "" when neither. Chain time then only
// jumps while every node is idle and the test is waiting, the way a discrete
// event simulation advances its clock.
func (d *Devnet) harnessHold() string {
	if why := d.netFaults.active(); why != "" {
		return why
	}
	if !testWaiting() {
		return "test running"
	}
	return ""
}

// leadGain is how far a block that came gap after the previous one moves the test
// clock's lead: the 3s of chain time it stands for less the wall time it took,
// never negative.
func leadGain(gap time.Duration) time.Duration {
	return max(0, hiveBlockInterval-gap)
}
