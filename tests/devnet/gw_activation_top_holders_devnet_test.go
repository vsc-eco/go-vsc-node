package devnet

import (
	"context"
	"sort"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestGatewayActivationTopHoldersOffline: mainnet's vsc.gateway at 0.12
// activation. Mainnet runs line 0.3 with STAKE-weighted gateway keys (10-31:
// 16 keys; the top 2 hold 48% of the weight, the top 4 over 2/3, threshold 2/3,
// no backstop). 0.12 switches elections to flat weights, so the first gateway
// rotation after activation installs equal key weights, but that rotation is
// signed under the OLD stake-weighted authority. If the top two holders are
// offline when it is due, nothing can sign it (or any withdrawal), while L2,
// now counting members not stake, keeps producing. This mirrors the mainnet
// shape on 10 nodes: stakes 9000/7000/5000/3500/2000x6 (top 2 = 44%, top 4 = 67%).
//
//	P0  line 0.3, stake-weighted authority installed (precondition: top 2 > 1/3).
//	P1  baseline withdrawal paid.
//	P2  0.12 activates; magi-1/2 (the top two) stop right after. Expect: no
//	    rotation, the withdrawal not paid, L2 blocks still produced.
//	P3  magi-1 returns: the rotation to equal weights lands, the withdrawal pays.
//
//	go test -v -run TestGatewayActivationTopHoldersOffline -timeout 150m ./tests/devnet/
func TestGatewayActivationTopHoldersOffline(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	cfg.Nodes = 10
	cfg.SkipFunding = false
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor, cp.ConsensusVersionFloorConsensus, cp.ConsensusVersionFloorEpoch = 0, 3, 1
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	// A wide rotation interval leaves a window between the activation election
	// and the first rotation under it (mainnet rotates hourly, elects 6-hourly).
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "200"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"
	d, ctx := startDevnetNoKey(t, cfg, 140*time.Minute)

	if err := d.waitForElectionEpoch(ctx, 1, 2, 20*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: no epoch 2: %v", err)
	}
	// P0: skew the stakes like mainnet.
	extra := map[int]string{1: "7000.000", 2: "5000.000", 3: "3000.000", 4: "1500.000"}
	for n, amt := range extra {
		if _, err := d.Deposit(ctx, n, amt, "hive"); err != nil {
			t.Fatalf("PRECONDITION FAILED: deposit HIVE for magi-%d: %v", n, err)
		}
	}
	for n := range extra {
		if !waitBalancePositive(t, d, ctx, 1, "hive:"+d.witnessAccount(n), "hive", 5*time.Minute) {
			t.Fatalf("PRECONDITION FAILED: magi-%d's HIVE deposit never credited", n)
		}
	}
	for n, amt := range extra {
		if _, err := d.ConsensusStake(n, n, amt); err != nil {
			t.Fatalf("PRECONDITION FAILED: stake for magi-%d: %v", n, err)
		}
	}
	shares := func() (thr int, total int, byAcct map[string]int) {
		thr, weights := gwActiveWeights(t, d)
		owners := gwKeyOwners(t, d, ctx)
		byAcct = map[string]int{}
		for k, w := range weights {
			byAcct[owners[k]] += w
			total += w
		}
		return thr, total, byAcct
	}
	top2Share := func() float64 {
		_, total, by := shares()
		if total == 0 {
			return 0
		}
		return float64(by[d.witnessAccount(1)]+by[d.witnessAccount(2)]) / float64(total)
	}
	// Wait for an election that carries the new stakes and a rotation that installs them.
	for end := devnetNow().Add(40 * time.Minute); top2Share() <= 1.0/3; devnetSleep(30 * time.Second) {
		if devnetNow().After(end) {
			thr, total, by := shares()
			t.Fatalf("PRECONDITION FAILED: the stake-weighted authority never put the top two over 1/3 (threshold %d of %d, %v)", thr, total, by)
		}
	}
	thr, total, by := shares()
	t.Logf("RESULT GW-A0: line 0.%d, threshold %d of %d, weights %v, top-2 share %.0f%%", vfActiveConsensus(d, ctx), thr, total, by, 100*top2Share())

	// P1 baseline, paid by a small holder.
	payer, payeeAcct := 6, d.witnessAccount(7)
	if _, err := d.Deposit(ctx, payer, "10.000", "hbd"); err != nil {
		t.Fatalf("PRECONDITION FAILED: HBD deposit: %v", err)
	}
	if !waitBalancePositive(t, d, ctx, 3, "hive:"+d.witnessAccount(payer), "hbd", 5*time.Minute) {
		t.Fatalf("PRECONDITION FAILED: HBD deposit never credited")
	}
	withdraw := func(label string, within time.Duration) (int64, bool) {
		before := gwL1Hbd(t, d, payeeAcct)
		tx, err := d.Withdraw(payer, payeeAcct, "1.000", "hbd", label)
		if err != nil {
			t.Fatalf("%s: withdraw: %v", label, err)
		}
		start := devnetNow()
		for end := devnetNow().Add(within); devnetNow().Before(end); devnetSleep(10 * time.Second) {
			if gwL1Hbd(t, d, payeeAcct) >= before+1000 {
				t.Logf("RESULT %s: paid after %s (tx %s)", label, devnetSince(start).Round(time.Second), tx)
				return before, true
			}
		}
		t.Logf("RESULT %s: NOT paid within %s (tx %s)", label, within, tx)
		return before, false
	}
	if _, ok := withdraw("GW-A1 baseline", 15*time.Minute); !ok {
		t.Fatalf("baseline withdrawal not paid before the activation")
	}

	// P2: activate 0.12, then the top two stop.
	epoch := custodyCurrentEpoch(d, ctx, 3)
	proposed := false
	for w := 3; w <= cfg.Nodes && !proposed; w++ {
		_, err := d.proposeConsensusVersion(w, 0, 12, epoch+2)
		proposed = err == nil
	}
	if !proposed {
		t.Fatalf("no witness could propose 0.12")
	}
	for end := devnetNow().Add(30 * time.Minute); vfActiveConsensus(d, ctx) < 12; devnetSleep(5 * time.Second) {
		if devnetNow().After(end) {
			t.Fatalf("0.12 never activated (line 0.%d)", vfActiveConsensus(d, ctx))
		}
	}
	mark := gwRotationMark(t, d)
	for _, n := range []int{1, 2} {
		if err := d.StopNode(ctx, n); err != nil {
			t.Errorf("stop magi-%d: %v", n, err)
		}
	}
	thr, total, by = shares()
	t.Logf("RESULT GW-A2: 0.12 active; magi-1/2 stopped; authority still threshold %d of %d, %v", thr, total, by)
	h0 := blockHeaderCount(t, d, ctx, 3)
	before, paid := withdraw("GW-A2 withdrawal with the top two offline", 15*time.Minute)
	h1 := blockHeaderCount(t, d, ctx, 3)
	rotated := gwRotationMark(t, d) > mark
	t.Logf("RESULT GW-A2: rotation landed=%v, withdrawal paid=%v, L2 block headers on magi-3 %d -> %d", rotated, paid, h0, h1)

	// P3: one top holder returns.
	if err := d.StartNode(ctx, 1); err != nil {
		t.Errorf("start magi-1: %v", err)
	}
	recovered := paid
	for end := devnetNow().Add(25 * time.Minute); devnetNow().Before(end) && !recovered; devnetSleep(10 * time.Second) {
		recovered = gwL1Hbd(t, d, payeeAcct) >= before+1000
	}
	thr, total, by = shares()
	equal := equalWeights(by)
	t.Logf("RESULT GW-A3: after magi-1 returned: withdrawal paid=%v, rotation landed=%v, authority threshold %d of %d, equal weights=%v %v",
		recovered, gwRotationMark(t, d) > mark, thr, total, equal, by)
	t.Logf("SUMMARY: with the top two holders offline at 0.12 activation the gateway rotated=%v and paid=%v while L2 headers went %d -> %d; one holder back: paid=%v, equal weights=%v",
		rotated, paid, h0, h1, recovered, equal)
	if !recovered {
		t.Errorf("the gateway did not recover after a top holder returned")
	}
}

// blockHeaderCount is the number of L2 block headers in a node's database.
func blockHeaderCount(t *testing.T, d *Devnet, ctx context.Context, node int) int64 {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return -1
	}
	defer client.Disconnect(ctx)
	n, err := client.Database(d.nodeDbName(node)).Collection("block_headers").CountDocuments(ctx, bson.M{})
	if err != nil {
		return -1
	}
	return n
}

// equalWeights reports whether every listed holder has the same weight.
func equalWeights(by map[string]int) bool {
	ws := make([]int, 0, len(by))
	for _, w := range by {
		ws = append(ws, w)
	}
	sort.Ints(ws)
	return len(ws) > 0 && ws[0] == ws[len(ws)-1]
}
