package devnet

// WQ-1 withdrawal queue on a real devnet: real nodes, real TSS, regtest bitcoind.
// From activateKey until the old generation's sweep settles every coin is on the
// retiring generation, so a withdrawal used to be refused for the whole window. With
// WQ-1 the contract queues it (debits an escrow, builds nothing, signs nothing) and the
// settle that frees the coins builds it through the normal unmap path, signed by the
// ACTIVE key (node S3 unchanged). These tests check that on the real path: nothing is
// signed while queued, the drained withdrawal pays the user exactly, the books balance
// (I1, I2), and every node holds the same queue and vault state after each phase.

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// wq1Keys is the state compared across nodes: supply, registry, pending spends, sweep
// list, and the queue bounds plus its first entries.
var wq1Keys = []string{"s", "r", "p", "msl", "wq", "wq-0", "wq-1", "wq-2", "wq-3"}

// wq1Rotated brings a devnet to the rotation window: gen-0 funded with `fund` sats for
// `user`, v2 on, gen-1 created, registered and activated, fee reserve funded. Returns the
// contract id and gen-1's primary key.
func wq1Rotated(t *testing.T, label string, fund int64) (*Devnet, context.Context, context.CancelFunc, string, string, string) {
	t.Helper()
	requireDocker(t)
	ctx, cancel := devnetWithTimeout(context.Background(), vfTestBudget(45*time.Minute))
	wasm := os.Getenv("BTC_MAPPING_WASM_PATH")
	if wasm == "" {
		cancel()
		t.Fatal("BTC_MAPPING_WASM_PATH must point at the btc-mapping-contract regtest wasm with WQ-1")
	}
	if _, err := os.Stat(wasm); err != nil {
		cancel()
		t.Fatalf("wasm: %v", err)
	}
	const hpin = 400
	cfg := vfSlowReshareConfig()
	cfg.SkipFunding = false
	cfg.EnableBitcoind = true
	cfg.SysConfigOverrides.ConsensusParams.VaultRotationV2ActivationHeight = hpin
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}
	d, _ := startDevnetNoKey(t, cfg, 45*time.Minute)

	seedH, err := d.MineBlocks(ctx, 101)
	if err != nil {
		t.Fatalf("mine: %v", err)
	}
	hdr1, _ := btcBlockHeaderHex(ctx, d, seedH)
	cid, err := d.DeployContract(ctx, ContractDeployOpts{
		WasmPath: wasm, Name: "btc-mapping-contract", Description: label, DeployerNode: 1, GQLNode: 2,
	})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	t.Logf("CONTRACT=%s", cid)
	vstatus(t, d, ctx, 1, cid, "seedBlocks", fmt.Sprintf(`{"block_header":"%s","block_height":%d}`, hdr1, seedH))
	d.WriteOracleConfigs(ctx)
	d.SetOracleContractIDs(map[string]string{"BTC": cid})
	d.RestartAllMagiNodes(ctx)
	devnetSleep(10 * time.Second)

	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd0, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-main", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen0 keygen: %v", err)
	}
	vfWaitPreparams(t, d, ctx, 12*time.Minute)
	if s := vfRegisterGenesis(t, d, ctx, 1, cid,
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, kd0.PublicKey, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen0 register: %s", s)
	}
	user := "hive:" + d.witnessAccount(1)
	fundVaultViaSPV(t, d, ctx, cid, kd0.PublicKey, backupPubKeyG, user, fund, seedH)
	if !balanceCredited(t, d, ctx, cid, user) {
		t.Fatalf("gen-0 funding failed")
	}
	vfWaitV2On(t, d, ctx, uint64(hpin))

	vstatus(t, d, ctx, 1, cid, "createKey", "")
	kd1, err := d.WaitForTssKey(ctx, 2, bson.M{"id": cid + "-mainv1", "status": "active"}, 8*time.Minute)
	if err != nil {
		t.Fatalf("gen1 keygen: %v", err)
	}
	if s := vstatus(t, d, ctx, 1, cid, "registerPublicKey",
		fmt.Sprintf(`{"primary_public_key":"%s","backup_public_key":"%s"}`, kd1.PublicKey, backupPubKeyG)); !isOK(s) {
		t.Fatalf("gen1 register: %s", s)
	}
	// VR2-12: activation now also waits for the pending key's first reshare (~one rotate
	// interval after the check-signature), so the budget is scaled like every other wait.
	activated := false
	attempts := int(math.Ceil(40 * vfTimeoutScale()))
	for i := 0; i < attempts && !activated; i++ {
		if isOK(vstatus(t, d, ctx, 1, cid, "activateKey", "")) {
			activated = true
			break
		}
		devnetSleep(15 * time.Second)
	}
	if !activated {
		t.Fatalf("gen-1 never activated (BRK-2 check-sig)")
	}
	fundFeeReserve(t, d, ctx, cid, kd1.PublicKey, backupPubKeyG, 10_000_000)
	return d, ctx, cancel, cid, kd1.PublicKey, user
}

// wq1Bounds decodes the queue bounds from one node (0,0 when absent).
func wq1Bounds(t *testing.T, d *Devnet, ctx context.Context, node int, cid string) (uint32, uint32, bool) {
	t.Helper()
	st, err := getStateHex(d, ctx, node, cid, []string{"wq"})
	if err != nil {
		return 0, 0, false
	}
	raw := st["wq"]
	if len(raw) == 0 {
		return 0, 0, true
	}
	if len(raw) != 8 {
		t.Errorf("CASE WQ1-STATE FAIL: wq is %d bytes on node %d, want 8", len(raw), node)
		return 0, 0, false
	}
	return binary.BigEndian.Uint32(raw[0:4]), binary.BigEndian.Uint32(raw[4:8]), true
}

const wq1NoCheck = math.MinInt64

// wq1WaitTail waits until node 2 has applied every request (queue tail == want), so an
// absence check that follows reads a node that is not lagging the caller.
func wq1WaitTail(t *testing.T, d *Devnet, ctx context.Context, cid string, want uint32) bool {
	t.Helper()
	deadline := devnetNow().Add(time.Duration(float64(3*time.Minute) * vfTimeoutScale()))
	for devnetNow().Before(deadline) {
		if _, tl, ok := wq1Bounds(t, d, ctx, 2, cid); ok && tl == want {
			return true
		}
		devnetSleep(3 * time.Second)
	}
	return false
}

// wq1EntryLive reports whether queue entry seq still holds a record on node 2.
func wq1EntryLive(t *testing.T, d *Devnet, ctx context.Context, cid string, seq int) bool {
	t.Helper()
	k := "wq-" + strconv.Itoa(seq)
	st, err := getStateHex(d, ctx, 2, cid, []string{k})
	if err != nil {
		t.Errorf("read %s: %v", k, err)
		return true
	}
	return len(st[k]) > 0
}

// wq1Books checks I1 (Sigma r == Active + Fee + over) and I2 (Active == User) on node 2.
// over is what the vault is knowingly over-collateralised by: queued escrow plus the
// debits of withdrawals in flight. wq1NoCheck only logs the gap.
// wq1WaitFeeCredited waits until the fee reserve the harness funded is reflected in
// FeeSupply, so the starting conservation gap is 0 and not the transient where the reserve
// coin is indexed in the registry but FeeSupply has not been credited yet.
func wq1WaitFeeCredited(t *testing.T, d *Devnet, ctx context.Context, cid string, min int64) {
	t.Helper()
	deadline := devnetNow().Add(time.Duration(float64(3*time.Minute) * vfTimeoutScale()))
	for devnetNow().Before(deadline) {
		if vf11ReadSupply(d, ctx, 2, cid).fee >= min {
			return
		}
		devnetSleep(5 * time.Second)
	}
	t.Logf("WARNING: fee reserve not credited to FeeSupply within the wait; the baseline gap may be transient")
}

func wq1Books(t *testing.T, d *Devnet, ctx context.Context, cid, phase string, over int64) (vf11Supply, int64) {
	t.Helper()
	s := vf11ReadSupply(d, ctx, 2, cid)
	st, err := getStateHex(d, ctx, 2, cid, []string{"r"})
	if !s.readable || err != nil {
		t.Errorf("CASE WQ1-BOOKS FAIL (%s): supply or registry unreadable (%v)", phase, err)
		return s, 0
	}
	var sum int64
	reg := st["r"]
	for off := 0; off+8 <= len(reg); off += 8 {
		var amt int64
		for _, b := range reg[off+2 : off+8] {
			amt = amt<<8 | int64(b)
		}
		sum += amt
	}
	gap := sum - (s.active + s.fee)
	if s.active != s.user {
		t.Errorf("CASE WQ1-I2 FAIL (%s): Active %d != User %d", phase, s.active, s.user)
	}
	if over != wq1NoCheck && gap != over {
		t.Errorf("CASE WQ1-I1 FAIL (%s): Sigma(r) %d - (Active %d + Fee %d) = %d, want %d", phase, sum, s.active, s.fee, gap, over)
	} else {
		t.Logf("CASE WQ1-BOOKS (%s): Sigma(r)=%d Active=%d User=%d Fee=%d gap=%d", phase, sum, s.active, s.user, s.fee, gap)
	}
	return s, gap
}

// wq1SameEverywhere waits until every node holds byte-identical queue and vault state.
func wq1SameEverywhere(t *testing.T, d *Devnet, ctx context.Context, cid, phase string) {
	t.Helper()
	deadline := devnetNow().Add(time.Duration(float64(3*time.Minute) * vfTimeoutScale()))
	var last string
	for devnetNow().Before(deadline) {
		ref, err := getStateHex(d, ctx, 1, cid, wq1Keys)
		if err != nil {
			devnetSleep(5 * time.Second)
			continue
		}
		fp := vfFingerprint(ref)
		same := true
		for n := 2; n <= d.cfg.Nodes; n++ {
			st, err := getStateHex(d, ctx, n, cid, wq1Keys)
			if err != nil || vfFingerprint(st) != fp {
				same = false
				if err == nil {
					last = fmt.Sprintf("node %d differs on %v", n, vfDiffKeysOf(ref, st, wq1Keys))
				}
				break
			}
		}
		if same {
			t.Logf("CASE WQ1-CONSENSUS PASS (%s): %d nodes hold identical queue/vault state %s", phase, d.cfg.Nodes, fp)
			return
		}
		devnetSleep(5 * time.Second)
	}
	t.Errorf("CASE WQ1-CONSENSUS FAIL (%s): nodes never agreed (%s)", phase, last)
}

func vfDiffKeysOf(a, b map[string][]byte, keys []string) []string {
	var out []string
	for _, k := range keys {
		if string(a[k]) != string(b[k]) {
			out = append(out, k)
		}
	}
	return out
}

// wq1Received is what a regtest address received with at least one confirmation, in sats.
func wq1Received(t *testing.T, d *Devnet, ctx context.Context, addr string) int64 {
	t.Helper()
	out, err := d.bitcoinCli(ctx, "getreceivedbyaddress", addr, "1")
	if err != nil {
		t.Errorf("getreceivedbyaddress %s: %v", addr, err)
		return -1
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(out), 64)
	if err != nil {
		t.Errorf("parse %q: %v", out, err)
		return -1
	}
	return int64(math.Round(f * 1e8))
}

// wq1NewSpends waits up to `within` for pending spends not in `before`.
func wq1NewSpends(t *testing.T, d *Devnet, ctx context.Context, cid string, before []string, want int, within time.Duration) []string {
	t.Helper()
	deadline := devnetNow().Add(within)
	var fresh []string
	for devnetNow().Before(deadline) {
		fresh = fresh[:0]
		for _, id := range txSpendIds(t, d, ctx, cid) {
			if !contains(before, id) {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) >= want {
			return fresh
		}
		devnetSleep(3 * time.Second)
	}
	return fresh
}

// TestVaultWQ1QueueAcrossRotation: three withdrawals in the rotation window are queued
// (nothing signed), one is cancelled, the sweep settle builds the other two through the
// normal unmap path with the ACTIVE key, both pay exactly, the books close, and every
// node agrees at every phase.
func TestVaultWQ1QueueAcrossRotation(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_WQ1_RUN") == "" {
		t.Skip("set VAULT_WQ1_RUN=1")
	}
	d, ctx, cancel, cid, primary1, _ := wq1Rotated(t, "wq1 rotation", 100_000_000)
	defer cancel()
	// The conservation gap before any withdrawal; every later phase is checked against it.
	wq1WaitFeeCredited(t, d, ctx, cid, 9_000_000) // the reserve is 10M; wait so the baseline gap is stable
	_, g0 := wq1Books(t, d, ctx, cid, "window open", wq1NoCheck)
	if g0 != 0 {
		t.Logf("NOTE: conservation gap %d before any withdrawal (later phases are checked against it)", g0)
	}
	baseSpends := txSpendIds(t, d, ctx, cid)

	// One queued withdrawal per account (F-1), so three distinct accounts. node 1 holds the
	// gen-0 credit; move enough to nodes 2 and 3 by an L2 transfer (no UTXO moves, so the
	// books are unchanged) for their withdrawal plus fee.
	acct := []string{"hive:" + d.witnessAccount(1), "hive:" + d.witnessAccount(2), "hive:" + d.witnessAccount(3)}
	nodes := []int{1, 2, 3}
	dest := []string{mustNewBtcAddr(t, d, ctx), mustNewBtcAddr(t, d, ctx), mustNewBtcAddr(t, d, ctx)}
	// Each above the 10M fee-reserve coin fundFeeReserve put on gen-1 (the only active-gen
	// coin in the window), so none can be served from it: all three must queue.
	amt := []int64{20_000_000, 15_000_000, 12_000_000}
	for i := 1; i < 3; i++ {
		if s := vstatus(t, d, ctx, 1, cid, "transfer", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, amt[i]+1_000_000, acct[i])); !isOK(s) {
			t.Fatalf("funding transfer to %s failed: %s", acct[i], s)
		}
	}
	balBefore := make([]int64, 3)
	for i := range acct {
		balBefore[i] = balanceSats(t, d, ctx, cid, acct[i])
	}
	for i := range amt {
		s := vstatus(t, d, ctx, nodes[i], cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, amt[i], dest[i]))
		if !isOK(s) {
			t.Fatalf("CASE WQ1-QUEUE FAIL: withdrawal %d in the rotation window was refused (status %s); before WQ-1 it was always refused", i, s)
		}
	}
	if !wq1WaitTail(t, d, ctx, cid, 3) {
		t.Fatalf("CASE WQ1-QUEUE FAIL: node 2 never showed 3 queued withdrawals")
	}
	if fresh := wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 20*time.Second); len(fresh) != 0 {
		t.Errorf("CASE WQ1-QUEUE FAIL: a queued withdrawal produced a pending spend %v (it must build nothing)", fresh)
	}
	head, tail, ok := wq1Bounds(t, d, ctx, 2, cid)
	if !ok || head != 0 || tail != 3 {
		t.Fatalf("CASE WQ1-QUEUE FAIL: queue bounds head=%d tail=%d ok=%v, want 0,3", head, tail, ok)
	}
	var escrow int64
	for i := range acct {
		e := balBefore[i] - balanceSats(t, d, ctx, cid, acct[i])
		if e < amt[i] || e > amt[i]+40_000 {
			t.Errorf("CASE WQ1-ESCROW FAIL: account %d escrow %d for amount %d (want amount + a small fee ceiling)", i, e, amt[i])
		}
		escrow += e
	}
	wq1Books(t, d, ctx, cid, "3 queued", g0+escrow)
	wq1SameEverywhere(t, d, ctx, cid, "3 queued")
	t.Logf("CASE WQ1-QUEUE PASS: 3 withdrawals queued (one per account) in the window, escrow %d, nothing built", escrow)

	// Cancel the third (account 3's, seq 2): its escrow comes back to account 3, nothing else moves.
	if s := vstatus(t, d, ctx, nodes[2], cid, "cancelUnmap", "2"); !isOK(s) {
		t.Errorf("CASE WQ1-CANCEL FAIL: cancelUnmap 2 status %s", s)
	}
	if got := balanceSats(t, d, ctx, cid, acct[2]); got != balBefore[2] {
		t.Errorf("CASE WQ1-CANCEL FAIL: account 3 balance %d after cancel, want its full %d back", got, balBefore[2])
	}
	wq1Books(t, d, ctx, cid, "after cancel", wq1NoCheck)
	wq1SameEverywhere(t, d, ctx, cid, "after cancel")

	// The sweep settles; the same confirmSpend builds the head. Each later settle frees change
	// and builds the next one.
	migrateAndSettle(t, d, ctx, cid, cid+"-main", primary1, backupPubKeyG)
	first := wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 60*time.Second)
	if len(first) == 0 {
		t.Fatalf("CASE WQ1-DRAIN FAIL: the sweep settle built no queued withdrawal")
	}
	if wq1EntryLive(t, d, ctx, cid, 0) {
		t.Errorf("CASE WQ1-DRAIN FAIL: entry 0 (the head) still queued after the sweep settle")
	}
	wq1Books(t, d, ctx, cid, "head built, in flight", wq1NoCheck)
	wq1SameEverywhere(t, d, ctx, cid, "head built")
	t.Logf("CASE WQ1-DRAIN PASS: the sweep settle built the queued head %v", first)

	// Real path for every drained withdrawal: the ACTIVE key signs (S3 admits it), broadcast,
	// mine, settle; each settle may drain the next entry.
	settled := map[string]bool{}
	for round := 0; round < 6; round++ {
		var fresh []string
		for _, id := range wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 30*time.Second) {
			if !settled[id] {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) == 0 {
			break
		}
		for _, tx := range fresh {
			settled[tx] = true
			if cs := settlePendingUnmap(t, d, ctx, cid, cid+"-mainv1", tx); !isOK(cs) {
				t.Errorf("CASE WQ1-SETTLE FAIL: drained withdrawal %s settle status %s", tx, cs)
			}
		}
	}
	if len(settled) != 2 {
		t.Errorf("CASE WQ1-SETTLE FAIL: %d withdrawals were built and settled, want exactly 2 (the third was cancelled)", len(settled))
	}
	for seq := 0; seq < 3; seq++ {
		if wq1EntryLive(t, d, ctx, cid, seq) {
			t.Errorf("CASE WQ1-DRAIN FAIL: queue entry %d still holds a record at the end", seq)
		}
	}
	for i := 0; i < 2; i++ {
		if got := wq1Received(t, d, ctx, dest[i]); got != amt[i] {
			t.Errorf("CASE WQ1-PAYOUT FAIL: %s received %d sats, want exactly %d", dest[i], got, amt[i])
		} else {
			t.Logf("CASE WQ1-PAYOUT PASS: %s received exactly %d sats", dest[i], got)
		}
	}
	if got := wq1Received(t, d, ctx, dest[2]); got != 0 {
		t.Errorf("CASE WQ1-CANCEL FAIL: the cancelled withdrawal's address received %d sats", got)
	}
	for i := 0; i < 2; i++ {
		spent := balBefore[i] - balanceSats(t, d, ctx, cid, acct[i])
		if spent <= amt[i] || spent > amt[i]+50_000 {
			t.Errorf("CASE WQ1-FEES FAIL: account %d paid %d for %d withdrawn (want amount + one miner fee)", i, spent, amt[i])
		} else {
			t.Logf("CASE WQ1-FEES PASS: account %d paid %d (amount %d + miner fee %d)", i, spent, amt[i], spent-amt[i])
		}
	}
	if got := balanceSats(t, d, ctx, cid, acct[2]); got != balBefore[2] {
		t.Errorf("CASE WQ1-FEES FAIL: the cancelled account 3 paid %d, want 0 (full balance %d)", balBefore[2]-got, balBefore[2])
	}
	if h, tl, _ := wq1Bounds(t, d, ctx, 2, cid); h != tl {
		t.Errorf("CASE WQ1-CLEAN FAIL: queue bounds head %d tail %d after every settle, want empty", h, tl)
	}
	wq1Books(t, d, ctx, cid, "all settled", g0)
	wq1SameEverywhere(t, d, ctx, cid, "all settled")
	if left := txSpendIds(t, d, ctx, cid); len(left) != len(baseSpends) {
		t.Errorf("CASE WQ1-CLEAN FAIL: %d pending spends left, baseline %d", len(left), len(baseSpends))
	}
}

// TestVaultWQ1PausedSettleThenNextWithdrawalHeals: the sweep settles while paused (its
// drain is skipped), so after unpause the queue holds an entry, the coins are free and
// nothing is in flight. The next withdrawal must drain it (self-heal), not wait behind it.
func TestVaultWQ1PausedSettleThenNextWithdrawalHeals(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_WQ1_RUN") == "" {
		t.Skip("set VAULT_WQ1_RUN=1")
	}
	d, ctx, cancel, cid, primary1, user := wq1Rotated(t, "wq1 paused settle", 100_000_000)
	defer cancel()
	wq1WaitFeeCredited(t, d, ctx, cid, 9_000_000) // the reserve is 10M; wait so the baseline gap is stable
	_, g0 := wq1Books(t, d, ctx, cid, "window open", wq1NoCheck)
	if g0 != 0 {
		t.Logf("NOTE: conservation gap %d before any withdrawal (later phases are checked against it)", g0)
	}
	baseSpends := txSpendIds(t, d, ctx, cid)
	destQ, destN := mustNewBtcAddr(t, d, ctx), mustNewBtcAddr(t, d, ctx)
	if s := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, 12_000_000, destQ)); !isOK(s) {
		t.Fatalf("CASE WQ1-QUEUE FAIL: window withdrawal refused (status %s)", s)
	}
	if !wq1WaitTail(t, d, ctx, cid, 1) {
		t.Fatalf("precondition: node 2 never showed the queued withdrawal")
	}
	// migrateVault is pause-gated, so build the sweep first, then pause, then settle it
	// (confirmSpend of a pending spend and addBlocks are pause-exempt; the node signs
	// regardless of the contract's pause).
	_ = primary1
	if s := vstatus(t, d, ctx, 1, cid, "migrateVault", ""); !isOK(s) {
		t.Fatalf("migrateVault: %s", s)
	}
	sweeps := wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 3*time.Minute)
	if len(sweeps) != 1 {
		t.Fatalf("expected 1 migration sweep pending, got %v", sweeps)
	}
	if s := vstatus(t, d, ctx, 1, cid, "pause", ""); !isOK(s) {
		t.Fatalf("pause: %s", s)
	}
	if cs := settleSweepByTxid(t, d, ctx, cid, cid+"-main", sweeps[0]); !isOK(cs) {
		t.Fatalf("the sweep did not settle while paused: %s", cs)
	}
	baseSpends = txSpendIds(t, d, ctx, cid)
	if fresh := wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 20*time.Second); len(fresh) != 0 {
		t.Errorf("CASE WQ1-PAUSE FAIL: something was built while paused: %v", fresh)
	}
	if s := vstatus(t, d, ctx, 1, cid, "unpause", ""); !isOK(s) {
		t.Fatalf("unpause: %s", s)
	}
	if h, tl, _ := wq1Bounds(t, d, ctx, 2, cid); tl-h != 1 {
		t.Fatalf("precondition: queue holds %d entries after the paused settle, want 1", tl-h)
	}
	wq1SameEverywhere(t, d, ctx, cid, "stranded")

	// The next withdrawal heals the queue: the stranded entry is built first.
	if s := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, 3_000_000, destN)); !isOK(s) {
		t.Fatalf("CASE WQ1-HEAL FAIL: next withdrawal status %s", s)
	}
	built := wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 60*time.Second)
	if len(built) == 0 {
		t.Fatalf("CASE WQ1-HEAL FAIL: nothing built after the next withdrawal; the queue stays stranded")
	}
	t.Logf("CASE WQ1-HEAL PASS: the next withdrawal drained the stranded entry (%d spends now in flight)", len(built))
	wq1SameEverywhere(t, d, ctx, cid, "healed")

	// Settle everything in flight (and anything the settles drain), then check payouts.
	settled := map[string]bool{}
	for round := 0; round < 4; round++ {
		var fresh []string
		for _, id := range wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 30*time.Second) {
			if !settled[id] {
				fresh = append(fresh, id)
			}
		}
		if len(fresh) == 0 {
			if h, tl, _ := wq1Bounds(t, d, ctx, 2, cid); h == tl {
				break
			}
			vstatus(t, d, ctx, 1, cid, "drainUnmapQueue", "")
			continue
		}
		for _, tx := range fresh {
			settled[tx] = true
			if cs := settlePendingUnmap(t, d, ctx, cid, cid+"-mainv1", tx); !isOK(cs) {
				t.Errorf("CASE WQ1-SETTLE FAIL: %s status %s", tx, cs)
			}
		}
	}
	if got := wq1Received(t, d, ctx, destQ); got != 12_000_000 {
		t.Errorf("CASE WQ1-PAYOUT FAIL: queued withdrawal paid %d, want 12000000", got)
	}
	if got := wq1Received(t, d, ctx, destN); got != 3_000_000 {
		t.Errorf("CASE WQ1-PAYOUT FAIL: next withdrawal paid %d, want 3000000", got)
	}
	if h, tl, _ := wq1Bounds(t, d, ctx, 2, cid); h != tl {
		t.Errorf("CASE WQ1-CLEAN FAIL: queue not empty at the end (head %d tail %d)", h, tl)
	}
	_ = user
	wq1Books(t, d, ctx, cid, "end", g0)
	wq1SameEverywhere(t, d, ctx, cid, "end")
}

// TestVaultWQ1NodeCrashDuringSigning: nodes crash while queued value is in the window.
// Two of five stop while the sweep is being signed (below quorum: nothing may move and the
// queued entry must stay intact); after they return the sweep settles and its drain builds
// the queued withdrawal; one node stops while that withdrawal is being signed (four of five
// still sign). The withdrawal pays exactly, the books close, and all five nodes agree.
func TestVaultWQ1NodeCrashDuringSigning(t *testing.T) {
	if testing.Short() {
		t.Skip("short mode")
	}
	if os.Getenv("VAULT_WQ1_RUN") == "" {
		t.Skip("set VAULT_WQ1_RUN=1")
	}
	d, ctx, cancel, cid, _, _ := wq1Rotated(t, "wq1 crash", 100_000_000)
	defer cancel()
	wq1WaitFeeCredited(t, d, ctx, cid, 9_000_000) // the reserve is 10M; wait so the baseline gap is stable
	_, g0 := wq1Books(t, d, ctx, cid, "window open", wq1NoCheck)
	baseSpends := txSpendIds(t, d, ctx, cid)
	dest := mustNewBtcAddr(t, d, ctx)
	const amt = int64(20_000_000) // above the 10M gen-1 fee-reserve coin, so it must queue
	if s := vstatus(t, d, ctx, 1, cid, "unmap", fmt.Sprintf(`{"amount":"%d","to":"%s"}`, amt, dest)); !isOK(s) {
		t.Fatalf("CASE WQ1-QUEUE FAIL: window withdrawal refused (status %s)", s)
	}
	if !wq1WaitTail(t, d, ctx, cid, 1) {
		t.Fatalf("precondition: node 2 never showed the queued withdrawal")
	}

	if s := vstatus(t, d, ctx, 1, cid, "migrateVault", ""); !isOK(s) {
		t.Fatalf("migrateVault: %s", s)
	}
	sweeps := wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 3*time.Minute)
	if len(sweeps) != 1 {
		t.Fatalf("expected 1 migration sweep pending, got %v", sweeps)
	}

	// Two of five down while the sweep waits for its signatures.
	vfStopNodes(t, d, ctx, []int{4, 5})
	devnetSleep(90 * time.Second)
	for _, n := range []int{1, 2, 3} {
		if _, tl, ok := wq1Bounds(t, d, ctx, n, cid); !ok || tl != 1 {
			t.Errorf("CASE WQ1-CRASH FAIL: magi-%d lost the queued entry during the outage (tail %d ok %v)", n, tl, ok)
		}
	}
	vfStartNodes(t, d, ctx, []int{4, 5})
	if cs := settleSweepByTxid(t, d, ctx, cid, cid+"-main", sweeps[0]); !isOK(cs) {
		t.Fatalf("CASE WQ1-CRASH FAIL: the sweep did not settle after the nodes returned: %s", cs)
	}
	built := wq1NewSpends(t, d, ctx, cid, baseSpends, 1, 60*time.Second)
	if len(built) != 1 {
		t.Fatalf("CASE WQ1-CRASH FAIL: the settle built %d withdrawals, want 1", len(built))
	}
	t.Logf("CASE WQ1-CRASH PASS(partial): entry intact through a quorum outage; built after recovery %v", built)

	// One of five down while the drained withdrawal is signed: four still reach the threshold.
	vfStopNodes(t, d, ctx, []int{5})
	if cs := settlePendingUnmap(t, d, ctx, cid, cid+"-mainv1", built[0]); !isOK(cs) {
		t.Errorf("CASE WQ1-CRASH FAIL: the drained withdrawal did not settle with one node down: %s", cs)
	}
	vfStartNodes(t, d, ctx, []int{5})
	if got := wq1Received(t, d, ctx, dest); got != amt {
		t.Errorf("CASE WQ1-PAYOUT FAIL: received %d, want exactly %d", got, amt)
	} else {
		t.Logf("CASE WQ1-PAYOUT PASS: received exactly %d sats", got)
	}
	if h, tl, _ := wq1Bounds(t, d, ctx, 2, cid); h != tl {
		t.Errorf("CASE WQ1-CLEAN FAIL: queue not empty at the end (head %d tail %d)", h, tl)
	}
	wq1Books(t, d, ctx, cid, "end", g0)
	wq1SameEverywhere(t, d, ctx, cid, "end (all five, the restarted ones caught up)")
}
