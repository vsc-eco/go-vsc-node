package devnet

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"vsc-node/cmd/mapping-bot/chain"
	"vsc-node/lib/btcvault"

	"github.com/btcsuite/btcd/chaincfg"
	"go.mongodb.org/mongo-driver/bson"
)

// tss_sign_tranche_devnet_test.go: one migration tranche of about 70 inputs,
// signed by a five-node devnet, measured. Each input is its own TSS sign
// session started by one RunActions, so this is the load the sign-start
// schedule has to carry.
//
//	TSS_TRANCHE_RUN=1 BTC_MAPPING_WASM_PATH=... go test -v -run TestTssSignTrancheLoad -timeout 150m ./tests/devnet/
//
// TSS_TRANCHE_PARAMS sets extra TssParams fields by Go field name, for a build
// that has them, e.g. "StartBatchSize=4,StartBatchGap=5s". A named field the
// build lacks fails the run, so a run meant for one schedule never silently
// measures another.
//
// Recorded per run: inputs in the tranche, time until every input is signed,
// session timeouts and systemic-timeout suppressions in the node logs, blame
// commitments committed while it signed, the spread of each session's start
// time across nodes (when TSS_TRANCHE_START_MARK names the start log line),
// and the peak CPU of each node. TSS_TRANCHE_LOG_DIR keeps each node's
// TSS-relevant log lines past the devnet teardown.

const (
	trancheTxs        = 4
	trancheOutsPerTx  = 18
	trancheOutputSats = 12_000
)

func TestTssSignTrancheLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("TSS_TRANCHE_RUN") == "" {
		t.Skip("set TSS_TRANCHE_RUN=1")
	}
	requireDocker(t)
	budget := 140 * time.Minute
	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(budget))
	t.Cleanup(cancel)
	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if _, err := os.Stat(wasm); wasm == "" || err != nil {
		t.Fatalf("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm: %q %v", wasm, err)
	}

	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	// The v2 height pin also turns on the 0.11.0 rules on the nodes that have them.
	// A rollout rehearsal against older binaries must not pin (they would compute
	// different contract results); it relies on the version floor instead
	// (TSS_TRANCHE_NO_PIN=1 with DEVNET_FLOOR_CONSENSUS=10 and a vault wasm without
	// the 0.11.0 reshare gate).
	if os.Getenv("TSS_TRANCHE_NO_PIN") == "" {
		cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = custodyHpin
	}
	cfg.SysConfigOverrides.TssParams.RotateInterval = 60
	applied := trancheSetTssParams(t, cfg.SysConfigOverrides.TssParams, os.Getenv("TSS_TRANCHE_PARAMS"))
	// A rolling upgrade: TSS_TRANCHE_OLD_DIR builds the nodes in TSS_TRANCHE_OLD_NODES
	// (e.g. "4,5") from another tree of the same version, with the same sysconfig.
	if dir := os.Getenv("TSS_TRANCHE_OLD_DIR"); dir != "" {
		cfg.OldCodeSourceDir = dir
		cfg.OldCodeSysconfig = true
		for _, s := range strings.Split(os.Getenv("TSS_TRANCHE_OLD_NODES"), ",") {
			n, err := strconv.Atoi(strings.TrimSpace(s))
			if err != nil || n < 1 || n > 5 {
				t.Fatalf("TSS_TRANCHE_OLD_NODES: %q is not a node number", s)
			}
			cfg.OldCodeNodes = append(cfg.OldCodeNodes, n)
		}
		applied += fmt.Sprintf("; old code %s on nodes %v", dir, cfg.OldCodeNodes)
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, budget)
	env := vfSetup(t, d, ctx, wasm, custodyHpin, 0, "tss sign tranche load")
	vfWaitV2On(t, d, ctx, custodyHpin)
	vfPreconditionV2Active(t, d, ctx, 2, env.cid, custodyHpin)
	c := &vfCase{t: t}
	cid := env.cid

	// Gen 0 gets trancheTxs x trancheOutsPerTx small UTXOs, one transaction per
	// block, each mapped with one call (a single 72-output map would not fit a
	// Hive custom_json).
	for i := 0; i < trancheTxs; i++ {
		recips := make([]string, trancheOutsPerTx)
		for j := range recips {
			recips[j] = fmt.Sprintf("hive:tranche-%02d", i*trancheOutsPerTx+j)
		}
		trancheFundMany(t, d, ctx, cid, env.primary0, backupPubKeyG, recips, trancheOutputSats)
	}
	want := trancheTxs * trancheOutsPerTx
	n0 := 0
	for i := 0; i < 18; i++ {
		if n0 = vfGenUtxoCountOn(d, ctx, 2, cid, 0); n0 >= want {
			break
		}
		devnetSleep(10 * time.Second)
	}
	if n0 < want {
		t.Fatalf("PRECONDITION FAILED: gen 0 holds %d utxos, want %d", n0, want)
	}

	// The sweep's miner fee comes from the reserve; fund it before the rotation.
	fundFeeReserve(t, d, ctx, cid, env.primary0, backupPubKeyG, 10_000_000)
	if _, ok := vfRotate(t, d, ctx, cid, 1); !ok {
		t.Fatalf("PRECONDITION FAILED: gen-1 never activated")
	}
	// From 0.11.0 the contract builds the first sweep only after gen 1's first
	// reshare; wait for it here so the build is one call.
	key1 := cid + "-" + btcvault.VaultKeyName(1)
	reshared := false
	for end := devnetNow().Add(vfTestBudget(25 * time.Minute)); devnetNow().Before(end) && !reshared; devnetSleep(20 * time.Second) {
		e, kg, ok := custodyEpochs(d, ctx, 2, key1)
		reshared = ok && e > kg
	}
	if !reshared {
		t.Fatalf("PRECONDITION FAILED: gen 1's key was not reshared within the wait")
	}

	key0 := cid + "-" + btcvault.VaultKeyName(0)
	blamesBefore := trancheBlameCount(d, ctx, 2)
	sweepTxid, sd, buildStatus := vfBuildSweep(t, d, ctx, 1, cid)
	if sd == nil {
		t.Fatalf("PRECONDITION FAILED: migrateVault built no sweep (status %s)", buildStatus)
	}
	n := len(sd.UnsignedSigHashes)
	if n < 60 {
		t.Fatalf("PRECONDITION FAILED: the tranche has %d inputs, want at least 60", n)
	}
	t.Logf("tranche built: %d inputs; TssParams %s", n, applied)

	// Sign it, sampling progress and CPU.
	stopCPU := make(chan struct{})
	cpuDone := make(chan map[string]float64)
	go func() { cpuDone <- trancheSampleCPU(ctx, d.ProjectName(), stopCPU) }()

	start := devnetNow()
	pending := make(map[string]bool, n)
	for _, uh := range sd.UnsignedSigHashes {
		pending[fmt.Sprintf("%x", uh.SigHash)] = true
	}
	var firstAt, allAt time.Duration
	deadline := start.Add(vfTestBudget(45 * time.Minute))
	for len(pending) > 0 && devnetNow().Before(deadline) && ctx.Err() == nil {
		if reqs, err := d.GetTssRequests(ctx, 2, key0); err == nil {
			for _, r := range reqs {
				if r.Sig != "" && pending[r.Msg] {
					delete(pending, r.Msg)
					if firstAt == 0 {
						firstAt = devnetSince(start)
					}
				}
			}
		}
		if len(pending) == 0 {
			allAt = devnetSince(start)
			break
		}
		devnetSleep(10 * time.Second)
	}
	close(stopCPU)
	peakCPU := <-cpuDone
	blames := trancheBlameCount(d, ctx, 2) - blamesBefore

	timeouts, suppressed := 0, 0
	starts := map[string][]time.Time{}
	mark := os.Getenv("TSS_TRANCHE_START_MARK")
	keepDir := os.Getenv("TSS_TRANCHE_LOG_DIR")
	for node := 1; node <= 5; node++ {
		logs := trancheNodeLogs(ctx, d, node)
		if keepDir != "" {
			trancheKeepLog(t, keepDir, d.ProjectName(), node, logs)
		}
		timeouts += strings.Count(logs, "session timeout")
		suppressed += strings.Count(logs, "suppressing systemic timeout blame")
		if mark != "" {
			for sid, at := range trancheStartTimes(logs, mark) {
				starts[sid] = append(starts[sid], at)
			}
		}
	}
	skew := trancheSkew(starts)

	signedAll := len(pending) == 0
	detail := fmt.Sprintf("inputs=%d signed=%d/%d first=%s all=%s session-timeouts=%d systemic-suppressed=%d blame-commitments=%d start-skew=%s peak-cpu=%s params=%s",
		n, n-len(pending), n, firstAt.Round(time.Second), allAt.Round(time.Second), timeouts, suppressed, blames, skew, trancheFmtCPU(peakCPU), applied)
	c.rec("TRANCHE-SIGN", "every input of a ~70-input tranche is signed", signedAll, detail)
	c.rec("TRANCHE-QUIET", "no session timeout and no blame commitment while it signs", signedAll && timeouts == 0 && blames == 0, detail)

	// B3b: settle the signed MaxMigrationInputs sweep so confirmSpend actually RUNS at the cap.
	// This exercises the CONTRACT-side confirmSpend sizing (NI-4/RC-2 contract path) at n inputs:
	// settleSweepByTxid broadcasts, mines, relays headers, then calls confirmSpend and returns its
	// status. A too-large payload or over-budget confirmSpend would be refused here, so a CONFIRMED
	// settle plus gen-0 draining by exactly the tranche size (snapshot_before - n left) is the proof
	// the cap holds end to end on devnet. The drain is asserted against the gen-0 count sampled right
	// before the sweep -- NOT want -- because fundFeeReserve adds one untagged gen-0 UTXO after the
	// funding precondition (so gen-0 holds want+1=73, and one 70-input tranche leaves 3, not want-n=2;
	// a second tranche sweeps the remainder, which is the NI-4 tranching design, not an insufficiency).
	// NOTE: a test-issued confirmSpend sets its own RcLimit, so this does NOT exercise the mapping-bot's
	// dry-run RcLimit sizing (NI-3, bot-side) -- that stays code+testnet.
	if signedAll {
		gen0Before := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
		cs := settleTrancheSweepMeasured(t, d, ctx, cid, key0, sweepTxid, c)
		gen0After := vfGenUtxoCountOn(d, ctx, 2, cid, 0)
		c.rec("TRANCHE-SETTLE",
			"the MaxMigrationInputs sweep's confirmSpend SETTLES on devnet and gen-0 drains by exactly the tranche size",
			isOK(cs) && gen0After == gen0Before-n,
			fmt.Sprintf("confirmSpend=%s gen0_utxos_before=%d gen0_utxos_after=%d want_after=%d (=before - %d swept) tranche_inputs=%d", cs, gen0Before, gen0After, gen0Before-n, n, n))
	}
	c.summary("TestTssSignTrancheLoad")
}

// trancheSetTssParams sets the "Field=value,..." pairs on p by Go field name
// (int, uint and time.Duration fields). An unknown field fails the test.
func trancheSetTssParams(t *testing.T, p any, spec string) string {
	t.Helper()
	if strings.TrimSpace(spec) == "" {
		return "default"
	}
	v := reflect.ValueOf(p).Elem()
	var applied []string
	for _, kv := range strings.Split(spec, ",") {
		name, val, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			t.Fatalf("TSS_TRANCHE_PARAMS: %q is not Field=value", kv)
		}
		f := v.FieldByName(name)
		if !f.IsValid() || !f.CanSet() {
			t.Fatalf("TSS_TRANCHE_PARAMS: this build's TssParams has no field %q", name)
		}
		switch {
		case f.Type() == reflect.TypeOf(time.Duration(0)):
			dur, err := time.ParseDuration(val)
			if err != nil {
				t.Fatalf("TSS_TRANCHE_PARAMS %s: %v", name, err)
			}
			f.SetInt(int64(dur))
		case f.CanInt():
			x, err := strconv.ParseInt(val, 10, 64)
			if err != nil {
				t.Fatalf("TSS_TRANCHE_PARAMS %s: %v", name, err)
			}
			f.SetInt(x)
		case f.CanUint():
			x, err := strconv.ParseUint(val, 10, 64)
			if err != nil {
				t.Fatalf("TSS_TRANCHE_PARAMS %s: %v", name, err)
			}
			f.SetUint(x)
		default:
			t.Fatalf("TSS_TRANCHE_PARAMS %s: unsupported type %s", name, f.Type())
		}
		applied = append(applied, name+"="+val)
	}
	return strings.Join(applied, ",")
}

// trancheFundMany sends one regtest transaction paying sats to the deposit
// address of each recipient (one gen-0 UTXO each), mines it alone in a block,
// relays the headers and maps it with one call.
func trancheFundMany(t *testing.T, d *Devnet, ctx context.Context, cid, primaryHex, backupHex string, recipients []string, sats int64) {
	t.Helper()
	lastRelayed := contractLastHeight(t, d, ctx, cid)
	outs := make(map[string]string, len(recipients))
	instructions := make([]string, len(recipients))
	for i, r := range recipients {
		instructions[i] = "deposit_to=" + r
		addr, _, err := trancheAddrGen().GenerateDepositAddress(primaryHex, backupHex, instructions[i])
		if err != nil {
			t.Fatalf("derive deposit addr for %s: %v", r, err)
		}
		outs[addr] = fmt.Sprintf("%d.%08d", sats/1e8, sats%int64(1e8))
	}
	outsJSON, _ := json.Marshal(outs)
	txid, err := d.bitcoinCli(ctx, "sendmany", "", string(outsJSON))
	if err != nil {
		t.Fatalf("sendmany: %v", err)
	}
	txid = strings.TrimSpace(txid)
	h, err := d.MineBlocks(ctx, 1)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	bhash, _ := d.bitcoinCli(ctx, "getblockhash", fmt.Sprint(h))
	blockJSON, _ := d.bitcoinCli(ctx, "getblock", strings.TrimSpace(bhash), "1")
	var blk struct {
		Tx []string `json:"tx"`
	}
	if err := json.Unmarshal([]byte(blockJSON), &blk); err != nil || len(blk.Tx) != 2 || blk.Tx[1] != txid {
		t.Fatalf("PRECONDITION FAILED: funding block should hold coinbase + %s, got %v (%v)", txid, blk.Tx, err)
	}
	rawTx, _ := d.bitcoinCli(ctx, "getrawtransaction", txid)
	proofHex := reverseHexBytes(blk.Tx[0])
	if _, err := d.MineBlocks(ctx, vfDepositMaturityBlocks); err != nil {
		t.Fatalf("mine maturity blocks: %v", err)
	}
	for hh := lastRelayed + 1; hh <= h+uint64(vfDepositMaturityBlocks); hh++ {
		hx, err := btcBlockHeaderHex(ctx, d, hh)
		if err != nil {
			t.Fatalf("hdr %d: %v", hh, err)
		}
		if s := vstatus(t, d, ctx, 1, cid, "addBlocks", fmt.Sprintf(`{"blocks":"%s","latest_fee":10}`, hx)); !isOK(s) {
			t.Fatalf("addBlocks height %d status=%s", hh, s)
		}
	}
	instrJSON, _ := json.Marshal(instructions)
	payload := fmt.Sprintf(`{"tx_data":{"block_height":%d,"raw_tx_hex":"%s","merkle_proof_hex":"%s","tx_index":1},"instructions":%s}`,
		h, strings.TrimSpace(rawTx), proofHex, instrJSON)
	if s, tx := vstatusTx(t, d, ctx, 1, cid, "map", payload); !isOK(s) {
		t.Fatalf("PRECONDITION FAILED: map of %d outputs status=%s %s", len(recipients), s, vfCallResult(d, ctx, 1, tx))
	}
}

// trancheAddrGen derives regtest deposit addresses the way fundVaultViaSPV does.
func trancheAddrGen() *chain.BTCAddressGenerator {
	return &chain.BTCAddressGenerator{Params: &chaincfg.RegressionNetParams, BackupCSVBlocks: 2}
}

var trancheKeepRe = regexp.MustCompile(`sign party|session timeout|timeout result|blame|tss_sign|insufficient participants|excluding|readmit|RunActions|lock held`)

// trancheKeepLog writes the node's TSS-relevant log lines to dir, so a run's
// evidence outlives the devnet teardown.
func trancheKeepLog(t *testing.T, dir, project string, node int, logs string) {
	var b strings.Builder
	for _, line := range strings.Split(logs, "\n") {
		if trancheKeepRe.MatchString(line) {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	path := fmt.Sprintf("%s/tranche-%s-magi-%d.log", dir, project, node)
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Logf("keep log %s: %v", path, err)
	}
}

// trancheBlameCount counts the blame commitments node has stored.
func trancheBlameCount(d *Devnet, ctx context.Context, node int) int {
	cs, err := d.GetCommitments(ctx, node, bson.M{"type": "blame"})
	if err != nil {
		return -1
	}
	return len(cs)
}

// trancheNodeLogs returns the node's container log with docker timestamps.
func trancheNodeLogs(ctx context.Context, d *Devnet, node int) string {
	out, _ := exec.CommandContext(ctx, "docker", "logs", "--timestamps", d.containerName(node)).CombinedOutput()
	return string(out)
}

var trancheSessionRe = regexp.MustCompile(`sessionId[=":\s]+"?([A-Za-z0-9_.:\-]+)`)

// trancheStartTimes maps each session id to the docker timestamp of the first
// log line that contains mark and names it.
func trancheStartTimes(logs, mark string) map[string]time.Time {
	out := map[string]time.Time{}
	for _, line := range strings.Split(logs, "\n") {
		if !strings.Contains(line, mark) {
			continue
		}
		ts, rest, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, ts)
		if err != nil {
			continue
		}
		m := trancheSessionRe.FindStringSubmatch(rest)
		if m == nil {
			continue
		}
		if _, seen := out[m[1]]; !seen {
			out[m[1]] = at
		}
	}
	return out
}

// trancheSkew reports, over sessions seen on at least two nodes, the median
// and the largest spread between the first and last node's start.
func trancheSkew(starts map[string][]time.Time) string {
	var spreads []time.Duration
	for _, ts := range starts {
		if len(ts) < 2 {
			continue
		}
		lo, hi := ts[0], ts[0]
		for _, x := range ts[1:] {
			if x.Before(lo) {
				lo = x
			}
			if x.After(hi) {
				hi = x
			}
		}
		spreads = append(spreads, hi.Sub(lo))
	}
	if len(spreads) == 0 {
		return "n/a"
	}
	sort.Slice(spreads, func(i, j int) bool { return spreads[i] < spreads[j] })
	return fmt.Sprintf("median %s max %s over %d sessions", spreads[len(spreads)/2].Round(100*time.Millisecond),
		spreads[len(spreads)-1].Round(100*time.Millisecond), len(spreads))
}

// trancheSampleCPU samples docker stats for the project's magi containers
// every 10 s until stop closes, returning each container's peak CPU percent.
func trancheSampleCPU(ctx context.Context, project string, stop <-chan struct{}) map[string]float64 {
	peak := map[string]float64{}
	var mu sync.Mutex
	for {
		out, err := exec.CommandContext(ctx, "docker", "stats", "--no-stream", "--format", "{{.Name}} {{.CPUPerc}}").Output()
		if err == nil {
			for _, line := range strings.Split(string(out), "\n") {
				name, pct, ok := strings.Cut(strings.TrimSpace(line), " ")
				if !ok || !strings.HasPrefix(name, project) || !strings.Contains(name, "magi-") {
					continue
				}
				x, err := strconv.ParseFloat(strings.TrimSuffix(pct, "%"), 64)
				if err != nil {
					continue
				}
				mu.Lock()
				if x > peak[name] {
					peak[name] = x
				}
				mu.Unlock()
			}
		}
		select {
		case <-stop:
			return peak
		case <-ctx.Done():
			return peak
		case <-devnetAfter(10 * time.Second):
		}
	}
}

func trancheFmtCPU(peak map[string]float64) string {
	if len(peak) == 0 {
		return "n/a"
	}
	names := make([]string, 0, len(peak))
	for k := range peak {
		names = append(names, k)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, k := range names {
		parts = append(parts, fmt.Sprintf("%s:%.0f%%", k[strings.LastIndex(k, "magi-"):], peak[k]))
	}
	return strings.Join(parts, " ")
}
