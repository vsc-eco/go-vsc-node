//go:build evm_devnet

package devnet

import (
	"context"
	"fmt"
	"math/big"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	ethcommon "github.com/ethereum/go-ethereum/common"
	ethCrypto "github.com/ethereum/go-ethereum/crypto"
	"go.mongodb.org/mongo-driver/bson"
)

// FS15 sign capacity (owner 11-03: "test the signing capacity, make sure devnet
// isn't flattering us and add timing pressure").
//
// Mainnet today (POST-LAUNCH-FOLLOWUPS 18am, read 11-03): one TSS key; 66 of 80
// epochs reshared at the first try, 14 after a retry; one witness left out of
// 16% of the reshares; blames spread over 10 witnesses; the BTC key signed 84
// requests in its life. No network has signed at EVM volume: every sign request
// is its own TSS session (tss.go) and the EVM bridge keeps up to 8 withdrawals
// in flight (MaxInFlightWithdrawals).
//
// Conditions, unlike the devnet's usual ones:
//   - real 3 s Hive blocks and no pacemaker: nothing waits for the bot or the
//     harness, so the 50-block sign interval and the bot's rounds run on the clock;
//   - one weak node (BB_WEAK_NODE, default 5; 0 = none): CPU capped
//     (BB_WEAK_CPUS, default 0.5) and 300±100 ms to every other node;
//   - both vault keys (BTC, EVM) reshare on their own schedule meanwhile;
//   - load: BB_EVM_WITHDRAWALS (default 16, twice the in-flight cap) sent in one
//     burst, while BTC withdrawals run one after another.
//
// It measures and does not tune. It fails only if a withdrawal is never paid
// within the deadline; the rest is MEASURE lines.
func TestBothBridges_FS15_SignCapacity(t *testing.T) {
	bbSkipShort(t)
	weak := 5
	if v, err := strconv.Atoi(os.Getenv("BB_WEAK_NODE")); err == nil {
		weak = v
	}
	cpus := os.Getenv("BB_WEAK_CPUS")
	if cpus == "" {
		cpus = "0.5"
	}
	n := 16
	if v, err := strconv.Atoi(os.Getenv("BB_EVM_WITHDRAWALS")); err == nil && v > 0 {
		n = v
	}
	const deadline = 45 * time.Minute
	b := newBothEnv(t, 300*time.Minute, bbWeatherLevel("none"), true, func(c *Config) {
		c.FastBlocks = false
		c.RealCadence = true
		c.LogLevel += ",bp=trace" // "sig accepted account=..." per block signature
	})
	d, ctx := b.d, b.ctx

	if weak > 0 {
		if out, err := exec.CommandContext(ctx, "docker", "update", "--cpus", cpus, d.containerName(weak)).CombinedOutput(); err != nil {
			t.Fatalf("cap magi-%d to %s CPU: %v %s", weak, cpus, err, out)
		}
		for i := 1; i <= d.cfg.Nodes; i++ {
			if i != weak {
				if err := d.AddLatency(ctx, weak, i, 300, 100); err != nil {
					t.Fatalf("latency magi-%d <-> magi-%d: %v", weak, i, err)
				}
			}
		}
		b.note("weak node: magi-%d at %s CPU, 300±100 ms to every other node", weak, cpus)
	}

	h0, _ := getHeadBlock(d.HiveRPCEndpoint())
	slot0, _ := vfMaxSlotHeight(d, ctx, 1)
	evmKey, btcKey := b.e.amID+"-"+evmKeyName(0), b.btcKeyID(0)
	reqs0 := fs15SignRequests(t, d, ctx, evmKey, btcKey)

	// The burst: n withdrawals from the user, consecutive nonces, one right after
	// another (each nonce's predecessor is already pending).
	k, err := NewEthKey(fmt.Sprintf("%x", ethCrypto.FromECDSA(b.userKey)))
	if err != nil {
		t.Fatal(err)
	}
	nonce, err := d.FetchL2Nonce(ctx, b.e.gqlNode, k.DID.String())
	if err != nil {
		t.Fatalf("user nonce: %v", err)
	}
	amount := new(big.Int).Div(bbEth(1), big.NewInt(20)) // 0.05 ETH each
	type withdrawal struct {
		to   ethcommon.Address
		sent time.Time
		paid time.Duration // 0 = not paid
	}
	ws := make([]*withdrawal, 0, n)
	start := devnetNow()
	for i := 0; i < n; i++ {
		to := ethCrypto.PubkeyToAddress(mustKey(t).PublicKey)
		if _, err := d.CallContractEvmAt(ctx, b.e.gqlNode, k, b.e.amID, "unmapETH",
			fmt.Sprintf(`{"to":"%s","amount":"%s","asset":"eth"}`, to.Hex(), amount.String()), 5_000_000, nonce+uint64(i)); err != nil {
			t.Fatalf("withdrawal %d (nonce %d): %v", i, nonce+uint64(i), err)
		}
		ws = append(ws, &withdrawal{to: to, sent: devnetNow()})
	}
	b.note("%d EVM withdrawals of %s wei sent in %s", n, amount, devnetSince(start).Round(time.Second))

	// BTC withdrawals one after another while the EVM ones drain.
	var btcMu sync.Mutex
	var btcTimes []time.Duration
	btcFailed := 0
	stopBtc := make(chan struct{})
	btcDone := make(chan struct{})
	go func() {
		defer close(btcDone)
		for {
			select {
			case <-stopBtc:
				return
			case <-ctx.Done():
				return
			default:
			}
			s := devnetNow()
			ok := b.btcWithdraw(0, 100_000) != ""
			btcMu.Lock()
			if ok {
				btcTimes = append(btcTimes, devnetSince(s))
			} else {
				btcFailed++
			}
			btcMu.Unlock()
		}
	}()

	// Wait for every EVM withdrawal to be paid on L1.
	end := start.Add(deadline)
	for devnetNow().Before(end) {
		left := 0
		for _, w := range ws {
			if w.paid == 0 {
				if b.l1Balance(w.to).Sign() > 0 {
					w.paid = devnetSince(w.sent)
				} else {
					left++
				}
			}
		}
		if left == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-devnetAfter(15 * time.Second):
		}
	}
	window := devnetSince(start)
	close(stopBtc)
	select {
	case <-btcDone:
	case <-devnetAfter(15 * time.Minute):
		b.note("FS15: the BTC withdrawal in progress did not finish within 15 min of the EVM window")
	}

	// ---- measurements ----
	var paid []time.Duration
	unpaid := 0
	for _, w := range ws {
		if w.paid > 0 {
			paid = append(paid, w.paid)
		} else {
			unpaid++
		}
	}
	t.Logf("MEASURE EVM withdrawals paid %d of %d within %s (window %s)", len(paid), n, deadline, window.Round(time.Second))
	if len(paid) > 0 {
		t.Logf("MEASURE EVM withdrawal sent -> paid on L1: %s", fs15Quantiles(paid))
	}
	btcMu.Lock()
	t.Logf("MEASURE BTC withdrawals settled %d, failed %d", len(btcTimes), btcFailed)
	if len(btcTimes) > 0 {
		t.Logf("MEASURE BTC withdrawal unmap -> confirmed: %s", fs15Quantiles(btcTimes))
	}
	btcMu.Unlock()
	reqs1 := fs15SignRequests(t, d, ctx, evmKey, btcKey)
	for _, key := range []string{evmKey, btcKey} {
		t.Logf("MEASURE TSS sign requests %s: before %v, after %v", key, reqs0[key], reqs1[key])
	}
	h1, _ := getHeadBlock(d.HiveRPCEndpoint())
	slot1, _ := vfMaxSlotHeight(d, ctx, 1)
	t.Logf("MEASURE L2 slot height %d -> %d over %d Hive blocks (a slot is 10 blocks: ~%d possible)", slot0, slot1, h1-h0, (h1-h0)/10)
	fs15Commitments(t, d, ctx, uint64(h0))
	fs15BlockSigners(t, d)
	if unpaid > 0 {
		t.Errorf("FS15: %d of %d EVM withdrawals were not paid on L1 within %s\n%s", unpaid, n, deadline, strings.Join(b.notes, "\n"))
	}
	if btcFailed > 0 {
		t.Errorf("FS15: %d BTC withdrawal(s) failed during the load", btcFailed)
	}
}

// fs15Quantiles formats p50/p90/max of a duration sample.
func fs15Quantiles(ds []time.Duration) string {
	s := append([]time.Duration{}, ds...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	q := func(f float64) time.Duration { return s[int(f*float64(len(s)-1))] }
	return fmt.Sprintf("p50 %s p90 %s max %s (n=%d)", q(0.5).Round(time.Second), q(0.9).Round(time.Second), s[len(s)-1].Round(time.Second), len(s))
}

// fs15SignRequests counts node 1's TSS sign requests per status for each key.
func fs15SignRequests(t *testing.T, d *Devnet, ctx context.Context, keys ...string) map[string]map[string]int {
	t.Helper()
	out := map[string]map[string]int{}
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Logf("MEASURE sign requests unreadable: %v", err)
		return out
	}
	defer client.Disconnect(ctx)
	for _, k := range keys {
		out[k] = map[string]int{}
		cur, err := client.Database(d.nodeDbName(1)).Collection("tss_requests").Find(ctx, bson.M{"key_id": k})
		if err != nil {
			continue
		}
		var rows []struct {
			Status string `bson:"status"`
		}
		_ = cur.All(ctx, &rows)
		for _, r := range rows {
			out[k][r.Status]++
		}
	}
	return out
}

// fs15Commitments counts node 1's TSS commitments since block h0 per key and type.
func fs15Commitments(t *testing.T, d *Devnet, ctx context.Context, h0 uint64) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		return
	}
	defer client.Disconnect(ctx)
	cur, err := client.Database(d.nodeDbName(1)).Collection("tss_commitments").Find(ctx, bson.M{"block_height": bson.M{"$gte": h0}})
	if err != nil {
		t.Logf("MEASURE commitments unreadable: %v", err)
		return
	}
	var rows []struct {
		Type  string `bson:"type"`
		KeyId string `bson:"key_id"`
	}
	_ = cur.All(ctx, &rows)
	count := map[string]int{}
	for _, r := range rows {
		count[r.KeyId+" "+r.Type]++
	}
	keys := make([]string, 0, len(count))
	for k := range count {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		t.Logf("MEASURE TSS commitments in the window: %s = %d", k, count[k])
	}
}

var fs15SigAccepted = regexp.MustCompile(`sig accepted.* account=(\S+)`)

// fs15BlockSigners counts, over every node's log, whose block signature each
// leader accepted (every devnet BOTH block so far showed participation 4/5:
// this names the signer that is missing).
func fs15BlockSigners(t *testing.T, d *Devnet) {
	t.Helper()
	count := map[string]int{}
	for i := 1; i <= d.cfg.Nodes; i++ {
		out, err := exec.Command("docker", "logs", d.containerName(i)).CombinedOutput()
		if err != nil {
			continue
		}
		for _, m := range fs15SigAccepted.FindAllSubmatch(out, -1) {
			count[string(m[1])]++
		}
	}
	accts := make([]string, 0, len(count))
	for a := range count {
		accts = append(accts, a)
	}
	sort.Strings(accts)
	parts := make([]string, 0, len(accts))
	for _, a := range accts {
		parts = append(parts, fmt.Sprintf("%s=%d", a, count[a]))
	}
	t.Logf("MEASURE block signatures accepted by leaders, per signer: %s", strings.Join(parts, " "))
}
