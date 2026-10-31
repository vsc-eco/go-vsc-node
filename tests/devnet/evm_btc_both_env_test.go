//go:build evm_devnet

// Shared harness for the BOTH tests (/mnt/o/BTC-EVM-INTERACTION-2026-10-31/PLAN.md):
// one devnet running the BTC vault (vault failure-test setup: bitcoind regtest,
// v2 contract, funded gen 0) next to the EVM bridge (A01 setup: anvil, mock
// verifier, bridge, bot). FastBlocks by default (BB_REAL_CADENCE=1 for the real
// 3 s cadence A/B); the bot holds the pacemaker at real cadence while it works.
//
//	BTC_MAPPING_WASM_PATH=<btc-mapping-contract regtest wasm>
//	DEVNET_PREPARAMS_DIR=~/.cache/magi-devnet-preparams (default when present)
//	eval "$(tests/devnet/suite/evm-artifacts.sh)"
//	BB_WEATHER=none|low|mid|high   instability level (each test has a default)
//	go test -tags evm_devnet -v -run TestBothBridges_<name> -timeout 300m ./tests/devnet/
package devnet

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/modules/common/params"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"go.mongodb.org/mongo-driver/bson"
)

// bothDevnet starts the devnet with both bridges. floor13: pin consensus 0.13
// from epoch 1 (EVM rules live); false keeps the floor at 0.12 so a test can
// activate 0.13 mid-run (FS9). opts adjust the config last (FS12's old-code
// nodes).
func bothDevnet(t *testing.T, timeout time.Duration, floor13 bool, opts ...func(*Config)) (context.Context, *evmBridgeEnv, *vfEnv) {
	t.Helper()
	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if _, err := os.Stat(wasm); wasm == "" || err != nil {
		t.Fatalf("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm: %q %v", wasm, err)
	}
	cfg := evmBridgeConfig()
	cfg.EnableBitcoind = true
	cfg.SkipFunding = false
	if os.Getenv("BB_REAL_CADENCE") == "1" {
		cfg.RealCadence = true
	} else {
		cfg.FastBlocks = true
	}
	if cfg.PreParamsFixtureDir == "" && os.Getenv("DEVNET_PREPARAMS_DIR") == "" {
		if home, err := os.UserHomeDir(); err == nil {
			if dir := filepath.Join(home, ".cache", "magi-devnet-preparams"); dirHasFiles(dir) {
				cfg.PreParamsFixtureDir = dir
			}
		}
	}
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorEpoch = 0, 1
	cp.ConsensusVersionFloorConsensus = 12
	if floor13 {
		cp.ConsensusVersionFloorConsensus = 13
	}
	cp.VaultRotationV2ActivationHeight = custodyHpin
	if cfg.SysConfigOverrides.TssParams == nil {
		cfg.SysConfigOverrides.TssParams = &params.TssParams{}
	}
	cfg.SysConfigOverrides.TssParams.RotateInterval = 60
	// Witness BLS seeds = sha256("devnet-bls-"+witness), so devnetGatewayKeypair
	// re-derives the gateway multisig keys (FS4 halt, FS11 leaver's key).
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["DEVNET_DETERMINISTIC_BLS"] = "1"
	for _, o := range opts {
		o(cfg)
	}
	d, ctx := startDevnetNoKey(t, cfg, timeout)
	if err := bootstrapEvmOwner(ctx, d); err != nil {
		t.Fatal(err)
	}
	e := setupEvmBridgeOn(t, ctx, d)
	v := vfSetup(t, d, ctx, wasm, custodyHpin, 50_000_000, "BOTH: BTC vault next to the EVM bridge")
	vfWaitV2On(t, d, ctx, custodyHpin)
	// The EVM bridge map, as a01Devnet; vfSetup already set the BTC one.
	if d.cfg.SysConfigOverrides.OracleParams == nil {
		d.cfg.SysConfigOverrides.OracleParams = &params.OracleParams{}
	}
	d.cfg.SysConfigOverrides.OracleParams.EvmBridgeContracts = map[string]string{"ETH": e.amID}
	d.cfg.SysConfigOverrides.OracleParams.EvmBridgeFromHeight = map[string]uint64{"ETH": 0}
	if err := writeSysConfigOverrides(d.cfg, d.devnetDir); err != nil {
		t.Fatal(err)
	}
	if err := d.RestartAllMagiNodes(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pollUntil(ctx, 5*time.Minute, func() bool { _, _, err := d.LocalNodeInfo(ctx, e.gqlNode); return err == nil }); err != nil {
		t.Fatal("nodes did not come back after the bridge-map restart")
	}
	want := "0.12"
	if floor13 {
		want = "0.13"
	}
	if err := waitForConsensus(ctx, d, want, 15*time.Minute); err != nil {
		t.Fatal(err)
	}
	return ctx, e, v
}

func dirHasFiles(dir string) bool {
	es, err := os.ReadDir(dir)
	return err == nil && len(es) > 0
}

// ---- instability levels (PLAN section 3) ----

type bbOutage struct {
	node     int
	at, span time.Duration // from the cycle start
}

type bbWeather struct {
	name                string
	chaos               *ChaosConfig // the bot's L1 RPC; nil = direct
	latencyMs, jitterMs int          // between every node pair, whole cycle
	outages             []bbOutage
	feeSpike            time.Duration // ETH base fee spike once the EVM batch is built
	reorg               bool          // shallow reorg of the mined ETH sweep
	btcReorg            bool          // the block carrying the BTC sweep is reorged out and re-mined
	botRestart          bool          // the bot restarts during the spike
	proverPause         time.Duration // no new verifier headers from the migration start (late prover)
	load                int           // withdrawals per hour on EACH bridge during the migrations
	botFund             string        // the bot's HBD (its RC); default 50000.000
	hiveSlowMs          int           // magi-3 -> Hive API latency, whole cycle
	hiveCut             time.Duration // magi-2 cut off from the Hive API, from 1 min
}

func scaleChaos(c ChaosConfig, k float64, slowMax time.Duration) *ChaosConfig {
	clamp := func(p float64) float64 { return min(p*k, 0.9) }
	c.Refuse429, c.Refuse502, c.Drop, c.Lost, c.Slow, c.StaleHead = clamp(c.Refuse429), clamp(c.Refuse502), clamp(c.Drop), clamp(c.Lost), clamp(c.Slow), clamp(c.StaleHead)
	c.SlowMax = slowMax
	return &c
}

// bbWeatherLevel: none, low, mid, high. high briefly loses quorum (two of five
// nodes down at once) on purpose: a rotation must survive it, not just avoid it.
func bbWeatherLevel(def string) bbWeather {
	lvl := os.Getenv("BB_WEATHER")
	if lvl == "" {
		lvl = def
	}
	switch lvl {
	case "low":
		return bbWeather{name: lvl, chaos: scaleChaos(MainnetWeather(201), 1, 1500*time.Millisecond), latencyMs: 50, jitterMs: 10,
			outages: []bbOutage{{node: 5, at: time.Minute, span: 2 * time.Minute}}, load: 10, hiveSlowMs: 200}
	case "mid":
		return bbWeather{name: lvl, chaos: scaleChaos(MainnetWeather(202), 2, 4*time.Second), latencyMs: 300, jitterMs: 100,
			outages:  []bbOutage{{node: 5, at: time.Minute, span: 4 * time.Minute}, {node: 4, at: 8 * time.Minute, span: 4 * time.Minute}},
			feeSpike: 4 * time.Minute, reorg: true, btcReorg: true, botRestart: true,
			proverPause: 10 * time.Minute, load: 30, hiveSlowMs: 500, hiveCut: 3 * time.Minute}
	case "high":
		return bbWeather{name: lvl, chaos: scaleChaos(MainnetWeather(203), 4, 15*time.Second), latencyMs: 1000, jitterMs: 300,
			outages:  []bbOutage{{node: 5, at: 30 * time.Second, span: 8 * time.Minute}, {node: 4, at: 6 * time.Minute, span: 6 * time.Minute}},
			feeSpike: 10 * time.Minute, reorg: true, btcReorg: true, botRestart: true,
			proverPause: 30 * time.Minute, load: 60, botFund: "60.000", hiveSlowMs: 2000, hiveCut: 8 * time.Minute}
	}
	return bbWeather{name: "none"}
}

// ---- the environment ----

type bothEnv struct {
	t       *testing.T
	ctx     context.Context
	d       *Devnet
	e       *evmBridgeEnv
	v       *vfEnv
	w       bbWeather
	anvil   *Anvil
	rpc     *ChaosRPC
	botCfg  BotConfig
	bot     *Bot
	botMu   sync.Mutex
	oldLogs []string
	userKey *ecdsa.PrivateKey
	user    string // the user's did:pkh (ETH balance on the bridge, HBD for RC)
	vault0  ethcommon.Address

	mu         sync.Mutex
	proverHold time.Time // no header planted before this (late prover)
	timings    map[string]time.Duration
	notes      []string
	stop       context.CancelFunc
	meter      *bbMeter
	// botWaits counts the waits in flight on something only the bot brings
	// about (botPoll); while any is, the pacemaker holds real cadence.
	botWaits atomic.Int32
	// onSigning runs once, when the BTC sweep's signatures are requested
	// (cycleHooks.btcSigning).
	onSigning func()
}

func newBothEnv(t *testing.T, budget time.Duration, w bbWeather, floor13 bool, opts ...func(*Config)) *bothEnv {
	t.Helper()
	ctx, e, v := bothDevnet(t, budget, floor13, opts...)
	b := &bothEnv{t: t, ctx: ctx, d: e.d, e: e, v: v, w: w, timings: map[string]time.Duration{}}
	bctx, cancel := context.WithCancel(ctx)
	b.stop = cancel
	t.Cleanup(cancel)
	t.Logf("BOTH devnet up: BTC vault %s, EVM bridge %s, weather %s", v.cid, e.amID, w.name)
	b.startEVM(bctx)
	b.meter = startMeter(bctx, b)
	t.Cleanup(func() { b.meter.report(t) })
	return b
}

// startEVM brings the EVM side up as the A01 waves do: anvil (behind the
// weather's flaky RPC), relayer + vault operator = the bot, a user with an
// ETH balance on the bridge, a background L1 miner and header planter.
func (b *bothEnv) startEVM(bctx context.Context) {
	t, e, ctx := b.t, b.e, b.ctx
	b.anvil = StartAnvil(t, ctx, evmChainID, nextAnvilPort())
	t.Cleanup(b.anvil.Stop)
	rpcURL := b.anvil.URL
	if b.w.chaos != nil {
		r, err := StartChaosRPC(b.anvil.URL, *b.w.chaos)
		if err != nil {
			t.Fatal(err)
		}
		// The weather is the cycle's: setup runs clean (applyWeather turns the
		// faults on). BB-2's first run had them on from the start and the bot,
		// scanning ~2 blocks a minute under them, never reached the deposit.
		r.SetFaults(false)
		b.rpc = r
		rpcURL = r.URL
	}
	botKey := mustKey(t)
	botDID := evmDID(ethCrypto.PubkeyToAddress(botKey.PublicKey))
	if err := e.proposeExecute("registerRelayer", fmt.Sprintf(`{"account":"%s"}`, botDID)); err != nil {
		t.Fatalf("registerRelayer: %v", err)
	}
	fund := b.w.botFund
	if fund == "" {
		fund = "50000.000"
	}
	if err := e.fundDID(botDID, fund); err != nil {
		t.Fatalf("fund bot DID: %v", err)
	}
	e.botAttests = true
	vault0, err := e.tssVault(25 * time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	b.vault0 = vault0
	if err := b.anvil.SetBalance(ctx, vault0, bbEth(1)); err != nil {
		t.Fatal(err)
	}
	b.botCfg = BotConfig{EthRPC: rpcURL, ContractID: e.amID, GraphQLURL: b.d.GQLEndpoint(e.gqlNode),
		EthPrivHex: hex.EncodeToString(ethCrypto.FromECDSA(botKey)), NeedsFile: filepath.Join(t.TempDir(), "needs.json")}
	b.bot = StartBot(t, ctx, b.botCfg)
	// Real cadence while the test waits on the bot and while the bot is
	// working (it logs each step; a deposit's steps are up to ~15 s apart).
	// BB-1's first run held only within 2 s of a log line: the fast clock spent
	// the deposit wait's 15 min in 77 s of wall time, mid-pipeline.
	b.d.AddExternalHold("evm bot", func() bool {
		b.botMu.Lock()
		defer b.botMu.Unlock()
		return b.bot != nil && (b.botWaits.Load() > 0 || b.bot.BusyWithin(30*time.Second))
	})
	if ok, msg := e.ownerCall("setVaultOperator", botDID); !ok {
		t.Fatalf("setVaultOperator: %s", msg)
	}
	go b.l1Background(bctx)
	// A user with ETH on the bridge (and HBD for RC to withdraw it).
	b.userKey = mustKey(t)
	userAddr := ethCrypto.PubkeyToAddress(b.userKey.PublicKey)
	b.user = evmDID(userAddr)
	if err := b.anvil.SetBalance(ctx, userAddr, bbEth(20)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := b.anvil.SendETH(ctx, b.userKey, vault0, bbEth(10)); err != nil {
		t.Fatalf("user deposit: %v", err)
	}
	if err := e.fundDID(b.user, "10000.000"); err != nil {
		t.Fatalf("fund user DID: %v", err)
	}
	if err := b.botPoll(15*time.Minute, func() bool { return e.stateStr("a-"+b.user+"-eth") != "" }); err != nil {
		t.Fatalf("the user's ETH deposit was never mapped\nbot:\n%s", b.botLogs())
	}
}

// l1Background mines an anvil block every 12 s of chain time and stands in
// for the prover. It plants every block the bot lists in its NEEDS_FILE, as
// the A01 waves plant each deposit's and withdrawal's block (the bot only links
// a header down from the tip when it is within 32 blocks; past that it waits
// for a proof at the block it asked for), and the newest finalized header at
// most once a minute (one L1 call per ~20 Hive blocks: planting every block
// starves L2, SLOT-BOUNDARY-TX-1). The weather's proverHold (late prover)
// stops both. BB-1 runs 2-3 planted only the finalized block: the deposit
// waited 15 min for a header that never came.
func (b *bothEnv) l1Background(ctx context.Context) {
	var planted uint64
	done := map[uint64]bool{}
	lastPlant := devnetNow()
	for ctx.Err() == nil {
		_ = b.anvil.Mine(ctx, 1)
		b.mu.Lock()
		held := devnetNow().Before(b.proverHold)
		b.mu.Unlock()
		if !held {
			for _, x := range b.neededBlocks() {
				if done[x] {
					continue
				}
				if f, err := b.anvil.Finalized(ctx); err == nil && f < x {
					_ = b.anvil.Mine(ctx, 70) // anvil finalizes head - 64
				}
				if err := b.e.plantFinalized(b.anvil, x); err == nil {
					done[x] = true
				}
			}
			if devnetSince(lastPlant) >= time.Minute {
				if f, err := b.anvil.Finalized(ctx); err == nil && f > planted {
					if err := b.e.plantFinalized(b.anvil, f); err == nil {
						planted, lastPlant = f, devnetNow()
					}
				}
			}
		}
		select {
		case <-ctx.Done():
		case <-devnetAfter(12 * time.Second):
		}
	}
}

// neededBlocks reads the bot's NEEDS_FILE: the L1 blocks whose headers it is
// waiting for.
func (b *bothEnv) neededBlocks() []uint64 {
	raw, err := os.ReadFile(b.botCfg.NeedsFile)
	if err != nil {
		return nil
	}
	var n struct {
		Blocks []uint64 `json:"blocks"`
	}
	if json.Unmarshal(raw, &n) != nil {
		return nil
	}
	return n.Blocks
}

// botPoll is pollUntil for a condition only the bot brings about. The bot runs
// on wall time, so the pacemaker holds real cadence for the whole wait (the
// "evm bot" external hold reads botWaits); otherwise a fast devnet's clock
// spends the budget in a fraction of the bot's time.
func (b *bothEnv) botPoll(budget time.Duration, cond func() bool) error {
	b.botWaits.Add(1)
	defer b.botWaits.Add(-1)
	return pollUntil(b.ctx, budget, cond)
}

func bbEth(n int64) *big.Int { return new(big.Int).Mul(big.NewInt(n), big.NewInt(1e18)) }

func (b *bothEnv) botLogs() string {
	b.botMu.Lock()
	defer b.botMu.Unlock()
	all := append(append([]string{}, b.oldLogs...), b.bot.Logs()...)
	return strings.Join(all, "\n")
}

func (b *bothEnv) restartBot() {
	b.botMu.Lock()
	b.bot.Stop()
	b.oldLogs = append(b.oldLogs, b.bot.Logs()...)
	b.botMu.Unlock()
	nb := StartBot(b.t, b.ctx, b.botCfg)
	b.botMu.Lock()
	b.bot = nb
	b.botMu.Unlock()
}

func (b *bothEnv) mark(name string, since time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.timings[name] = devnetSince(since)
	b.t.Logf("TIMING %-62s %s", name, b.timings[name].Round(time.Second))
}

func (b *bothEnv) note(format string, a ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := fmt.Sprintf(format, a...)
	b.notes = append(b.notes, s)
	b.t.Log("NOTE " + s)
}

// at runs fn after `after` of chain time from start, on its own goroutine
// (faults; it must not call t.Fatal).
func (b *bothEnv) at(start time.Time, after time.Duration, name string, fn func()) {
	go func() {
		wait := after - devnetSince(start)
		if wait > 0 {
			select {
			case <-b.ctx.Done():
				return
			case <-devnetAfter(wait):
			}
		}
		b.t.Logf("FAULT %s at +%s", name, devnetSince(start).Round(time.Second))
		fn()
	}()
}

// applyWeather schedules the level's node outages and inter-node latency.
func (b *bothEnv) applyWeather(start time.Time) {
	if b.rpc != nil {
		b.rpc.SetFaults(true)
	}
	if b.w.latencyMs > 0 {
		n := b.d.cfg.Nodes
		for i := 1; i <= n; i++ {
			for j := i + 1; j <= n; j++ {
				if err := b.d.AddLatency(b.ctx, i, j, b.w.latencyMs, b.w.jitterMs); err != nil {
					b.t.Logf("latency %d<->%d: %v", i, j, err)
				}
			}
		}
		b.t.Cleanup(func() {
			for i := 1; i <= b.d.cfg.Nodes; i++ {
				_ = b.d.RemoveLatency(context.Background(), i)
			}
		})
	}
	if b.w.hiveSlowMs > 0 {
		if ip := b.droneIP(); ip != "" {
			if err := b.d.addNetemForIP(b.ctx, b.d.containerName(3), ip, b.w.hiveSlowMs, b.w.hiveSlowMs/4); err != nil {
				b.t.Logf("magi-3 -> Hive API latency: %v", err)
			}
		}
	}
	if b.w.hiveCut > 0 {
		b.at(start, time.Minute, fmt.Sprintf("magi-2 cut off from the Hive API for %s", b.w.hiveCut), func() { b.hiveCut(2, b.w.hiveCut) })
	}
	for _, o := range b.w.outages {
		o := o
		b.at(start, o.at, fmt.Sprintf("magi-%d down for %s", o.node, o.span), func() { b.outage(o.node, o.span) })
	}
}

func (b *bothEnv) outage(node int, span time.Duration) {
	if err := b.d.StopNode(b.ctx, node); err != nil {
		b.t.Logf("stop magi-%d: %v", node, err)
		return
	}
	devnetSleep(span)
	if err := b.d.StartNode(b.ctx, node); err != nil {
		b.t.Errorf("restart magi-%d after its outage: %v", node, err)
	}
}

// ---- BTC primitives ----

// unstakeVerdict submits a 1.000 HIVE consensus_unstake from the node's witness
// and returns its terminal status and refusal text (TransactionRecord.error).
func (b *bothEnv) unstakeVerdict(node int) (string, string) {
	tx, err := b.d.ConsensusUnstake(node, "1.000")
	if err != nil {
		return "BROADCAST_ERR", err.Error()
	}
	var st, errText string
	_ = pollUntil(b.ctx, 10*time.Minute, func() bool {
		var out struct {
			FindTransaction []struct {
				Status string  `json:"status"`
				Error  *string `json:"error"`
			} `json:"findTransaction"`
		}
		q := `query($id:String!){findTransaction(filterOptions:{byId:$id}){status error}}`
		if b.d.gqlQuery(b.ctx, 1, q, map[string]any{"id": tx}, &out) != nil || len(out.FindTransaction) == 0 {
			return false
		}
		st = out.FindTransaction[0].Status
		if e := out.FindTransaction[0].Error; e != nil {
			errText = *e
		}
		return st == "CONFIRMED" || st == "FAILED"
	})
	return st, errText
}

// tssKeyExists: node 1 has the TSS key record (created by a contract's
// create_key, before its keygen).
func (b *bothEnv) tssKeyExists(id string) bool {
	client, err := b.d.mongoClient(b.ctx)
	if err != nil {
		return false
	}
	defer client.Disconnect(b.ctx)
	n, err := client.Database(b.d.nodeDbName(1)).Collection("tss_keys").CountDocuments(b.ctx, bson.M{"id": id})
	return err == nil && n > 0
}

func (b *bothEnv) btcKeyID(gen int) string {
	if gen == 0 {
		return b.v.cid + "-main"
	}
	return fmt.Sprintf("%s-mainv%d", b.v.cid, gen)
}

// btcMigrate sweeps the retiring generation into the active one: fee reserve,
// build, every input signed by the retiring key, broadcast + mine, relay +
// confirm. Returns the confirm status ("" when a step did not complete).
func (b *bothEnv) btcMigrate(retiring int, activePrimary string) string {
	t, d, ctx := b.t, b.d, b.ctx
	fundFeeReserve(t, d, ctx, b.v.cid, activePrimary, backupPubKeyG, 10_000_000)
	txid, sd, st := vfBuildSweep(t, d, ctx, 1, b.v.cid)
	if sd == nil {
		b.note("BTC sweep: no signing data (txid=%q migrateVault=%s)", txid, st)
		return ""
	}
	if f := b.onSigning; f != nil {
		b.onSigning = nil
		f()
	}
	// Real cadence for the signature wait: after a fault the restarted nodes
	// need wall-clock minutes (pubsub reconnect, readiness) while the fast clock
	// would spend the wait's budget in seconds. One vfAwaitSweepSignatures pass
	// is 2 min, shorter than FS2's 3-min halt (FS2 runs 2 and 3: "the retiring
	// key never signed every input" while magi-4/5 were still down), so passes
	// repeat for 15 min and the time to sign is recorded.
	b.botWaits.Add(1)
	signStart := devnetNow()
	var raw string
	ok := false
	for end := devnetNow().Add(15 * time.Minute); !ok && devnetNow().Before(end); {
		raw, ok = vfAwaitSweepSignatures(t, d, ctx, b.btcKeyID(retiring), sd)
	}
	b.botWaits.Add(-1)
	if !ok {
		b.note("BTC sweep %s: the retiring key never signed every input within 15 min", txid)
		return ""
	}
	b.mark("BTC sweep signed (from the request)", signStart)
	bc, h, err := vfBroadcastAndMine(t, d, ctx, raw)
	if err != nil {
		b.note("BTC sweep broadcast: %v", err)
		return ""
	}
	if b.w.btcReorg {
		h = b.btcReorgOut(bc, h)
	}
	return vfRelayAndConfirm(t, d, ctx, 1, b.v.cid, bc, h)
}

// btcReorgOut reorgs out the block that carries txid (F9/F25 shape: invalidate
// it, mine past it, the tx returns from the mempool) and returns its new height.
func (b *bothEnv) btcReorgOut(txid string, h uint64) uint64 {
	d, ctx := b.d, b.ctx
	hash, err := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	if err != nil {
		b.note("BTC reorg: getblockhash %d: %v", h, err)
		return h
	}
	if _, err := d.bitcoinCli(ctx, "invalidateblock", strings.TrimSpace(hash)); err != nil {
		b.note("BTC reorg: invalidateblock: %v", err)
		return h
	}
	if _, err := d.MineBlocks(ctx, 2); err != nil {
		b.note("BTC reorg: mine: %v", err)
		return h
	}
	out, err := d.bitcoinCli(ctx, "getrawtransaction", txid, "true")
	var tx struct {
		Blockhash string `json:"blockhash"`
	}
	if err != nil || json.Unmarshal([]byte(out), &tx) != nil || tx.Blockhash == "" {
		b.note("BTC reorg: sweep %s not re-mined after the reorg (%v)", txid, err)
		return h
	}
	hdr, err := d.bitcoinCli(ctx, "getblockheader", tx.Blockhash)
	var bh struct {
		Height uint64 `json:"height"`
	}
	if err != nil || json.Unmarshal([]byte(hdr), &bh) != nil {
		return h
	}
	b.note("BTC reorg: sweep %s moved from block %d to %d", txid, h, bh.Height)
	return bh.Height
}

// droneIP is the Hive API container's address on the devnet network.
func (b *bothEnv) droneIP() string {
	for _, name := range []string{b.d.projectName + "-drone-1", b.d.projectName + "-drone", "drone"} {
		if ip, err := b.d.containerIP(b.ctx, name); err == nil && ip != "" {
			return ip
		}
	}
	b.t.Logf("Hive API container not found: no Hive API faults this run")
	return ""
}

// hiveCut cuts one node off from the Hive API for span (a dead or lagging API node).
func (b *bothEnv) hiveCut(node int, span time.Duration) {
	ip := b.droneIP()
	if ip == "" {
		return
	}
	c := b.d.containerName(node)
	if err := b.d.iptables(b.ctx, c, "-A", "OUTPUT", "-d", ip, "-j", "DROP"); err != nil {
		b.t.Logf("cut magi-%d from the Hive API: %v", node, err)
		return
	}
	devnetSleep(span)
	if err := b.d.iptables(b.ctx, c, "-D", "OUTPUT", "-d", ip, "-j", "DROP"); err != nil {
		b.t.Errorf("restore magi-%d's Hive API access: %v", node, err)
	}
}

// ethReorgSweep waits for generation 0's first sweep (nonce 0 from vault 0) to
// mine and, if it is not final yet, reorgs it out; the bot must re-post it.
func (b *bothEnv) ethReorgSweep(fromBlock uint64) {
	for i := 0; i < 240 && b.ctx.Err() == nil; i++ {
		if _, blk, found, _ := b.anvil.FindTxFrom(b.ctx, b.vault0, 0, fromBlock); found {
			if f, _ := b.anvil.Finalized(b.ctx); f >= blk {
				b.note("ETH reorg: the sweep was already final in block %d", blk)
				return
			}
			head, err := b.anvil.cli.BlockNumber(b.ctx)
			if err != nil {
				return
			}
			if err := b.anvil.rpc.CallContext(b.ctx, nil, "anvil_reorg", head-blk+1, []any{}); err != nil {
				b.note("ETH reorg: anvil_reorg: %v", err)
				return
			}
			b.note("ETH reorg: the sweep's block %d reorged out (depth %d)", blk, head-blk+1)
			return
		}
		devnetSleep(5 * time.Second)
	}
}

// loadGen keeps `perHour` withdrawals an hour flowing on EACH bridge until
// stop: small EVM withdrawals to fresh addresses and BTC unmaps (the TSS signs
// them; they are not driven to L1 here, the point is the sign + queue load).
func (b *bothEnv) loadGen(perHour int, stop <-chan struct{}) {
	if perHour <= 0 {
		return
	}
	gap := time.Hour / time.Duration(perHour)
	k, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(b.userKey)))
	if err != nil {
		return
	}
	evmOK, evmErr, btcOK, btcErr := 0, 0, 0, 0
	defer func() {
		b.note("load: EVM withdrawals %d ok / %d refused, BTC unmaps %d ok / %d refused", evmOK, evmErr, btcOK, btcErr)
	}()
	for {
		select {
		case <-stop:
			return
		case <-b.ctx.Done():
			return
		case <-devnetAfter(gap):
		}
		to := ethCrypto.PubkeyToAddress(mustKey(b.t).PublicKey)
		if _, err := b.d.CallContractEvm(b.ctx, b.e.gqlNode, k, b.e.amID, "unmapETH",
			fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, to.Hex(), "20000000000000000"), 5_000_000); err != nil {
			evmErr++
		} else {
			evmOK++
		}
		dest, err := b.d.bitcoinCli(b.ctx, "getnewaddress")
		if err != nil {
			btcErr++
			continue
		}
		if st := vstatus(b.t, b.d, b.ctx, 1, b.v.cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, 20_000, strings.TrimSpace(dest))); isOK(st) {
			btcOK++
		} else {
			btcErr++
		}
	}
}

// btcWithdraw issues a withdrawal signed by gen's key and drives it to confirmSpend.
func (b *bothEnv) btcWithdraw(gen int, sats int64) string {
	t, d, ctx := b.t, b.d, b.ctx
	txid, _, raw := rhIssueUnmap(t, d, ctx, b.v.cid, b.btcKeyID(gen), sats)
	if raw == "" {
		b.note("BTC withdrawal %q: not signed by %s", txid, b.btcKeyID(gen))
		return ""
	}
	bc, h, err := vfBroadcastAndMine(t, d, ctx, raw)
	if err != nil {
		b.note("BTC withdrawal broadcast: %v", err)
		return ""
	}
	return vfRelayAndConfirm(t, d, ctx, 1, b.v.cid, bc, h)
}

// ---- EVM primitives ----

func evmKeyName(gen int) string {
	if gen == 0 {
		return "primary"
	}
	return fmt.Sprintf("primaryv%d", gen)
}

// evmWithdraw asks the bridge to send wei to a fresh L1 address; returns it.
func (b *bothEnv) evmWithdraw(wei *big.Int) ethcommon.Address {
	to, _ := b.evmWithdrawTx(wei)
	return to
}

// evmWithdrawTx is evmWithdraw that also returns the L2 transaction id.
func (b *bothEnv) evmWithdrawTx(wei *big.Int) (ethcommon.Address, string) {
	to := ethCrypto.PubkeyToAddress(mustKey(b.t).PublicKey)
	k, err := NewEthKey(hex.EncodeToString(ethCrypto.FromECDSA(b.userKey)))
	if err != nil {
		b.t.Fatal(err)
	}
	tx, err := b.d.CallContractEvm(b.ctx, b.e.gqlNode, k, b.e.amID, "unmapETH",
		fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, to.Hex(), wei.String()), 5_000_000)
	if err != nil {
		b.t.Fatalf("unmapETH: %v", err)
	}
	return to, tx
}

// userEth is the user's ETH balance on the bridge (wei, the first field of
// its a-<did>-eth record; "" when absent).
func (b *bothEnv) userEth() string {
	return strings.SplitN(b.e.stateStr("a-"+b.user+"-eth"), "|", 2)[0]
}

func (b *bothEnv) l1Balance(addr ethcommon.Address) *big.Int {
	var h string
	if err := b.anvil.rpc.CallContext(b.ctx, &h, "eth_getBalance", addr, "latest"); err != nil {
		return big.NewInt(0)
	}
	v, _ := new(big.Int).SetString(strings.TrimPrefix(h, "0x"), 16)
	if v == nil {
		return big.NewInt(0)
	}
	return v
}

// evmMigrated: the bot's batch is confirmed (no open sweep records) and gen 0
// is purged. A purge only follows inactivation, and the bot does not always log
// the inactivation result (FS6 with 68335e76: "retire: purged=0" alone), so the
// purge is the signal.
func (b *bothEnv) evmMigrated() bool {
	return b.e.stateStr("msr") == "" && strings.Contains(b.botLogs(), "purged=0")
}

// ---- the cycle ----

// Faults that take L2 down belong in btcKeygen / btcSigning: they fire while the
// network itself works (keygens, sweep signing, the bot's batch), not while the
// harness makes its own owner calls, which single-bridge helpers treat as fatal
// when L2 is down (FS1/FS3 first runs: "unmap rejected (INCLUDED)", "fee-reserve
// addBlocks: INCLUDED").
type cycleHooks struct {
	afterCreateKeys func(start time.Time) // EVM keygen requested
	btcKeygen       func()                // the BTC next-generation key is created, EVM keygen still pending
	afterActivate   func()                // both new generations active
	migrating       func()                // both migrations started
	btcSigning      func()                // the BTC sweep's signatures requested, the EVM batch running
	btcSwept        func()                // the BTC sweep confirmed, the EVM batch maybe still running
	settled         func()                // everything settled
}

// runCycle is one full rotation of BOTH vaults in the same window:
// EVM keygen requested, a BTC withdrawal signed meanwhile, the BTC rotation
// (gen 0 -> 1), EVM activation, an EVM withdrawal queued during the migration,
// the BTC sweep while the bot runs the EVM batch, a BTC withdrawal from gen 1
// while the EVM sweeps settle, then the EVM retire + purge. Faults come from
// the weather and the hooks. Each step is timed; L2 must keep producing.
func (b *bothEnv) runCycle(h cycleHooks) {
	t, e := b.t, b.e
	start := devnetNow()
	b.applyWeather(start)
	if ok, msg := e.ownerCall("createKey", "{}"); !ok {
		t.Fatalf("EVM createKey: %s", msg)
	}
	if h.afterCreateKeys != nil {
		h.afterCreateKeys(start)
	}
	if h.btcKeygen != nil {
		go func() {
			if err := pollUntil(b.ctx, 30*time.Minute, func() bool { return b.tssKeyExists(b.btcKeyID(1)) }); err == nil {
				b.note("BTC key %s created at +%s", b.btcKeyID(1), devnetSince(start).Round(time.Second))
				h.btcKeygen()
			}
		}()
	}
	b.onSigning = h.btcSigning
	if st := b.btcWithdraw(0, 100_000); st != "" {
		b.mark("BTC gen-0 withdrawal settled (EVM keygen running)", start)
	}
	p1, ok := vfRotate(t, b.d, b.ctx, b.v.cid, 1)
	if !ok {
		t.Fatalf("BTC rotation gen 0 -> 1 did not complete next to the EVM rotation (%s in)\n%s", devnetSince(start).Round(time.Second), strings.Join(b.notes, "\n"))
	}
	b.mark("BTC rotation gen 0 -> 1 activated", start)
	if err := pollUntil(b.ctx, 45*time.Minute, func() bool { return e.keyPub(evmKeyName(1)) != "" }); err != nil {
		t.Fatalf("EVM keygen never landed next to the BTC rotation: %v\nbot:\n%s", err, b.botLogs())
	}
	b.mark("EVM createKey -> keygen", start)
	if err := b.botPoll(60*time.Minute, func() bool { ok, _ := e.ownerCall("activateKey", "{}"); return ok }); err != nil {
		t.Fatalf("EVM activateKey never accepted next to the BTC rotation: %v\nbot:\n%s", err, b.botLogs())
	}
	b.mark("EVM createKey -> activateKey", start)
	if h.afterActivate != nil {
		h.afterActivate()
	}

	// Both migrations at once, with a withdrawal queued on the EVM side.
	mig := devnetNow()
	migBlock, _ := b.anvil.cli.BlockNumber(b.ctx)
	recip := b.evmWithdraw(bbEth(1))
	if b.w.proverPause > 0 {
		b.mu.Lock()
		b.proverHold = mig.Add(b.w.proverPause)
		b.mu.Unlock()
		b.note("late prover: no new verifier headers for %s from the migration start", b.w.proverPause)
	}
	if b.w.reorg {
		go b.ethReorgSweep(migBlock)
	}
	stopLoad := make(chan struct{})
	go b.loadGen(b.w.load, stopLoad)
	defer close(stopLoad)
	if h.migrating != nil {
		h.migrating()
	}
	if b.w.feeSpike > 0 {
		b.at(mig, 90*time.Second, "ETH base fee spike "+b.w.feeSpike.String(), func() { b.feeSpike(b.w.feeSpike) })
	}
	if st := b.btcMigrate(0, p1); st == "" {
		t.Fatalf("BTC migration did not settle next to the EVM batch\n%s", strings.Join(b.notes, "\n"))
	}
	b.mark("BTC migration sweep confirmed (EVM batch running)", mig)
	if h.btcSwept != nil {
		h.btcSwept()
	}
	if st := b.btcWithdraw(1, 100_000); st != "" {
		b.mark("BTC gen-1 withdrawal settled (EVM sweeps settling)", mig)
	}
	if err := b.botPoll(60*time.Minute, func() bool { return b.l1Balance(recip).Sign() > 0 }); err != nil {
		t.Fatalf("the EVM withdrawal queued during both migrations never reached L1\nbot:\n%s", b.botLogs())
	}
	b.mark("EVM withdrawal queued in the migration -> on L1", mig)
	if err := b.botPoll(60*time.Minute, b.evmMigrated); err != nil {
		t.Fatalf("EVM generation 0 never drained, retired and purged: msr=%q\nbot:\n%s", e.stateStr("msr"), b.botLogs())
	}
	b.mark("EVM batch confirmed, gen 0 retired + purged", mig)
	if h.settled != nil {
		h.settled()
	}
	b.mark("whole cycle", start)
}

// feeSpike holds the ETH base fee far above what the sweeps were priced at,
// restarting the bot in the middle when the weather says so.
func (b *bothEnv) feeSpike(span time.Duration) {
	spike := big.NewInt(60e9)
	end := devnetNow().Add(span)
	restarted := false
	for devnetNow().Before(end) && b.ctx.Err() == nil {
		_ = b.anvil.rpc.CallContext(b.ctx, nil, "anvil_setNextBlockBaseFeePerGas", "0x"+spike.Text(16))
		if b.w.botRestart && !restarted && devnetSince(end.Add(-span)) > span/2 {
			b.restartBot()
			restarted = true
		}
		devnetSleep(3 * time.Second)
	}
}

// ---- measurements (BB-5): L2 slots lost, Hive L1 density, GraphQL latency ----

type bbMeter struct {
	mu         sync.Mutex
	b          *bothEnv
	hive0      uint64
	slot0      int
	gqlSamples []time.Duration
	stopped    bool
}

func startMeter(ctx context.Context, b *bothEnv) *bbMeter {
	m := &bbMeter{b: b}
	h, _ := getHeadBlock(b.d.HiveRPCEndpoint())
	m.hive0 = uint64(h)
	m.slot0, _ = vfMaxSlotHeight(b.d, ctx, 1)
	go func() {
		for ctx.Err() == nil {
			t0 := time.Now() // wall: GraphQL latency as a bot sees it
			_, _, err := b.d.LocalNodeInfo(ctx, b.e.gqlNode)
			if err == nil {
				m.mu.Lock()
				m.gqlSamples = append(m.gqlSamples, time.Since(t0)) // wall: as above
				m.mu.Unlock()
			}
			select {
			case <-ctx.Done():
			case <-devnetAfter(20 * time.Second):
			}
		}
	}()
	return m
}

// hiveOpsDensity counts Hive blocks in [from, to) that carry at least one Magi
// L1 op (custom_json id "vsc.*"): an L1 Magi op in every block loses every L2
// slot (SLOT-BOUNDARY-TX-1).
func hiveOpsDensity(rpc string, from, to uint64) (withOps, total int) {
	for n := from; n < to; n++ {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"condenser_api.get_block","params":[%d]}`, n)
		resp, err := http.Post(rpc, "application/json", strings.NewReader(body))
		if err != nil {
			continue
		}
		var r struct {
			Result struct {
				Transactions []struct {
					Operations [][]json.RawMessage `json:"operations"`
				} `json:"transactions"`
			} `json:"result"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&r)
		resp.Body.Close()
		total++
		found := false
		for _, tx := range r.Result.Transactions {
			for _, op := range tx.Operations {
				if len(op) == 2 && strings.Contains(string(op[0]), "custom_json") && strings.Contains(string(op[1]), `"id":"vsc.`) {
					found = true
				}
			}
		}
		if found {
			withOps++
		}
	}
	return withOps, total
}

func (m *bbMeter) report(t *testing.T) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return
	}
	m.stopped = true
	samples := append([]time.Duration{}, m.gqlSamples...)
	m.mu.Unlock()
	b := m.b
	h1, _ := getHeadBlock(b.d.HiveRPCEndpoint())
	slot1, _ := vfMaxSlotHeight(b.d, context.Background(), 1)
	hiveBlocks := uint64(h1) - m.hive0
	withOps, total := hiveOpsDensity(b.d.HiveRPCEndpoint(), m.hive0, uint64(h1))
	t.Logf("METER hive blocks %d, L2 slot height %d -> %d (a slot is 10 blocks: ~%d slots possible)", hiveBlocks, m.slot0, slot1, hiveBlocks/10)
	if total > 0 {
		t.Logf("METER Hive blocks carrying a Magi L1 op: %d of %d (%.0f%%)", withOps, total, 100*float64(withOps)/float64(total))
	}
	if n := len(samples); n > 0 {
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		t.Logf("METER GraphQL localNodeInfo latency p50 %s p90 %s max %s (n=%d)", samples[n/2].Round(time.Millisecond), samples[n*9/10].Round(time.Millisecond), samples[n-1].Round(time.Millisecond), n)
	}
	t.Logf("METER blame lines on magi-1: %d", vfCountLogs(b.d, context.Background(), 1, "blame"))
	if logs, err := b.d.Logs(context.Background(), "magi-1"); err == nil {
		var ms []int
		for _, ln := range strings.Split(logs, "\n") {
			if !strings.Contains(ln, "Magi block module=") {
				continue
			}
			if i := strings.Index(ln, "elapsed_ms="); i >= 0 {
				var v int
				if _, err := fmt.Sscanf(ln[i+len("elapsed_ms="):], "%d", &v); err == nil {
					ms = append(ms, v)
				}
			}
		}
		if n := len(ms); n > 0 {
			sort.Ints(ms)
			t.Logf("METER L2 block apply (magi-1, elapsed_ms) p50 %d p90 %d max %d (n=%d; a Hive block is 3000)", ms[n/2], ms[n*9/10], ms[n-1], n)
		}
	}
	b.mu.Lock()
	names := make([]string, 0, len(b.timings))
	for k := range b.timings {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		t.Logf("TIMING %-62s %s", k, b.timings[k].Round(time.Second))
	}
	for _, n := range b.notes {
		t.Logf("NOTE %s", n)
	}
	b.mu.Unlock()
}
