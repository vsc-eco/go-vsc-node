package devnet

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// GW-PAY — gateway pay-once when the SETTLEMENT WRITE is lost, and no stranded
// withdrawal when the fix fail-stops on it.
//
// WHY THIS TEST EXISTS
//
// A gateway payout is protected from being paid twice by exactly one thing: the
// flip of its action to status "complete" (ledger.ExecuteComplete) when the
// mined `vsc.actions` header is ingested. GetPendingActions selects purely on
// status == "pending", and there is NO per-withdrawal in-flight lock — c9a86798
// added one and cc6b7d33 reverted it, because cosigners marked actions
// "processing" in their own DB and stranded the withdrawal forever when the
// leader's broadcast then failed (the bradleyarrow incident). The dedup test was
// rewritten at the same time to assert that executeActions does NOT mutate the
// DB, so from then on NOTHING tested re-pay.
//
// That left the double-pay path untested and, worse, silent: ExecuteComplete
// returned nothing and discarded its UpdateMany error, so a write failure left
// a PAID withdrawal "pending" and the gateway re-selected and re-paid it every
// action tick (~60 s) for as long as the failure lasted. 2026-05 on mainnet is
// the same shape by a different route: 34 duplicate action groups, 68 extra
// payouts, action 24f87dab paid 9x.
//
// The fix is NOT a new marker. ExecuteComplete now returns its error and the
// callers fail-stop on it (blockingRetry). That is also the circuit breaker, for
// free and with no new state: hive/block-consumer.ProcessBlock runs the sync
// block ticks and the state engine on ONE goroutine, so a node blocked on this
// write processes no further blocks and therefore fires no further gateway
// action ticks. A node that cannot record settlement stops paying. Nothing is
// marked unavailable, so no single withdrawal can be stranded by it — the node
// halts loudly and recovers when its DB does.
//
// WHAT IS INJECTED, AND WHY IT IS HONEST
//
// A document validator on the faulted node's own `ledger_actions` collection rejects
// any document with status "complete" (collMod + validationAction "error").
// ExecuteComplete's UpdateMany then fails with DocumentValidationFailure — the
// exact production shape: reads keep working, every other collection keeps
// working, new actions still store as "pending", and only the settlement write
// fails. It is applied per node DB, which the single global `failCommand`
// failpoint (see evm_bridge_cg1_readfault) cannot do: only one failCommand
// configuration is active per mongod, and this fault must hit a signing
// MAJORITY at once to be interesting.
//
// Why a majority: the leader builds the batch from its own DB and cosigners
// REBUILD it from theirs, signing only on a matching txid (gateway/p2p.go).
// A minority that lost the completion therefore cannot get a re-pay signed —
// its peers' batch excludes the paid action and the txids differ. The old
// code was already safe there. Paying twice needs >= 2/3 of the gateway weight
// to share the stale view, which is what a shared-infrastructure Mongo fault
// does. 7 of 9 nodes are faulted here for that reason.
//
// PHASES (9 nodes, floor 0.3, action interval 20 blocks)
//
//	P0 CONTROL   wd1 paid, then observed for 3 further action ticks: the payee
//	             is credited EXACTLY once. Nothing in the repo asserts this today.
//	P1 FAULT     the completion write fails on magi-1..7. wd2 is paid (the
//	             payout mines before settlement, so the fault cannot block it),
//	             then observed for 4 further ticks:
//	               with the fix: still exactly one credit, and the faulted nodes
//	                 stop advancing their processed height — the breaker engaged.
//	               before the fix (GWPAY_PREFIX=1): a second credit lands within
//	                 ~1 tick and the faulted nodes keep processing.
//	P2 RECOVER   the fault is cleared. The halted nodes resume, wd2's action is
//	             "complete" on ALL 9 nodes, and no action anywhere is in
//	             "processing" — the reverted marker's stranding shape.
//	P3 AFTER     wd3 is paid exactly once: fail-stopping did not wedge the
//	             gateway or cost a withdrawal.
//
// Run: go test -v -run TestGatewayPayOnceSettlementFault -timeout 120m ./tests/devnet/
// Differential against the pre-fix binary: GWPAY_PREFIX=1 GW_OLD_DIR=<pre-fix tree> ...

// gwPayActionsColl is the gateway's action collection. NOT "actions" -- that is a
// different collection used elsewhere in this harness; the bridge actions the
// gateway pays from live in "ledger_actions"
// (modules/db/vsc/ledger/ledger.go:530). Getting this wrong fails with
// NamespaceNotFound rather than silently arming nothing, which is the only
// reason the first run of this test did not report a false pass.
const gwPayActionsColl = "ledger_actions"

// gwPayFaultedNodes is the signing majority that loses the settlement write.
// 7 of 9 keys carry more than the gateway's ceil(2/3) weight threshold, so a
// batch they agree on is signable — which is what makes a re-pay possible at
// all.
var gwPayFaultedNodes = []int{1, 2, 3, 4, 5, 6, 7}

func gwPayPreFix() bool { return os.Getenv("GWPAY_PREFIX") != "" }

// gwPayCompletionFault makes the settlement write fail (on=true) or work again
// (on=false) on the given nodes, by rejecting any `actions` document that
// reaches status "complete".
func gwPayCompletionFault(t *testing.T, d *Devnet, ctx context.Context, nodes []int, on bool) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo connect for completion fault: %v", err)
	}
	defer client.Disconnect(ctx)
	for _, n := range nodes {
		ndb := client.Database(d.nodeDbName(n))
		if on {
			// Mongo creates a collection lazily on first write and collMod on an
			// absent namespace fails, so create it if this node has not written
			// an action yet. NamespaceExists is the normal case.
			if err := ndb.CreateCollection(ctx, gwPayActionsColl); err != nil &&
				!strings.Contains(err.Error(), "NamespaceExists") {
				t.Fatalf("ensuring %s.%s exists: %v", d.nodeDbName(n), gwPayActionsColl, err)
			}
		}
		cmd := bson.D{
			{Key: "collMod", Value: gwPayActionsColl},
			{Key: "validator", Value: bson.D{}},
			{Key: "validationLevel", Value: "off"},
		}
		if on {
			cmd = bson.D{
				{Key: "collMod", Value: gwPayActionsColl},
				{Key: "validator", Value: bson.D{
					{Key: "status", Value: bson.D{{Key: "$ne", Value: "complete"}}},
				}},
				{Key: "validationLevel", Value: "strict"},
				{Key: "validationAction", Value: "error"},
			}
		}
		if err := ndb.RunCommand(ctx, cmd).Err(); err != nil {
			// Clearing a fault on a collection that never existed is a no-op,
			// not a failure -- this also runs from t.Cleanup.
			if !on && strings.Contains(err.Error(), "NamespaceNotFound") {
				continue
			}
			t.Fatalf("collMod(%s, fault=%v) on %s: %v", gwPayActionsColl, on, d.nodeDbName(n), err)
		}
	}
	t.Logf("completion-write fault %v on nodes %v", on, nodes)
}

// gwPayVerifyFault proves the injection does what it claims: the settlement
// write fails on a faulted node, reads still work there, and an unfaulted node
// can still complete. Without this the whole test could pass on a fault that
// never armed.
func gwPayVerifyFault(t *testing.T, d *Devnet, ctx context.Context, faulted, healthy int) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	probe := bson.M{"id": "gwpay-probe", "status": "pending", "type": "withdraw", "amount": int64(1)}
	for _, n := range []int{faulted, healthy} {
		col := client.Database(d.nodeDbName(n)).Collection(gwPayActionsColl)
		if _, err := col.DeleteOne(ctx, bson.M{"id": "gwpay-probe"}); err != nil {
			t.Fatalf("probe cleanup on %s: %v", d.nodeDbName(n), err)
		}
		if _, err := col.InsertOne(ctx, probe); err != nil {
			t.Fatalf("a PENDING action must still insert on %s: %v", d.nodeDbName(n), err)
		}
		if err := col.FindOne(ctx, bson.M{"id": "gwpay-probe"}).Err(); err != nil {
			t.Fatalf("reads must keep working on %s: %v", d.nodeDbName(n), err)
		}
	}
	fCol := client.Database(d.nodeDbName(faulted)).Collection(gwPayActionsColl)
	hCol := client.Database(d.nodeDbName(healthy)).Collection(gwPayActionsColl)
	if _, err := fCol.UpdateOne(ctx, bson.M{"id": "gwpay-probe"}, bson.M{"$set": bson.M{"status": "complete"}}); err == nil {
		t.Fatalf("PRECONDITION FAILED: the completion write still succeeds on faulted %s — the fault never armed", d.nodeDbName(faulted))
	}
	if _, err := hCol.UpdateOne(ctx, bson.M{"id": "gwpay-probe"}, bson.M{"$set": bson.M{"status": "complete"}}); err != nil {
		t.Fatalf("the completion write must still succeed on healthy %s: %v", d.nodeDbName(healthy), err)
	}
	for _, n := range []int{faulted, healthy} {
		if _, err := client.Database(d.nodeDbName(n)).Collection(gwPayActionsColl).DeleteOne(ctx, bson.M{"id": "gwpay-probe"}); err != nil {
			t.Fatalf("probe cleanup on %s: %v", d.nodeDbName(n), err)
		}
	}
	t.Logf("fault verified: completion write fails on %s, succeeds on %s, reads fine on both",
		d.nodeDbName(faulted), d.nodeDbName(healthy))
}

// gwPayActionCounts returns the per-status count of withdraw actions on a node.
func gwPayActionCounts(t *testing.T, d *Devnet, ctx context.Context, node int) map[string]int {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)
	cur, err := client.Database(d.nodeDbName(node)).Collection(gwPayActionsColl).
		Find(ctx, bson.M{"type": "withdraw"})
	if err != nil {
		t.Fatalf("reading actions on %s: %v", d.nodeDbName(node), err)
	}
	defer cur.Close(ctx)
	out := map[string]int{}
	for cur.Next(ctx) {
		var doc struct {
			Status string `bson:"status"`
		}
		if err := cur.Decode(&doc); err != nil {
			t.Fatalf("decoding action on %s: %v", d.nodeDbName(node), err)
		}
		out[doc.Status]++
	}
	return out
}

func TestGatewayPayOnceSettlementFault(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	cfg.Nodes = 9 // MIN_GATEWAY_KEYS is 8: a smaller fleet never installs a gateway authority
	cfg.SkipFunding = false
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = 3
	cp.ConsensusVersionFloorEpoch = 1
	if cfg.MagiEnv == nil {
		cfg.MagiEnv = map[string]string{}
	}
	cfg.MagiEnv["VSC_GATEWAY_ROTATION_INTERVAL"] = "20"
	cfg.MagiEnv["VSC_GATEWAY_ACTION_INTERVAL"] = "20"
	if old := os.Getenv("GW_OLD_DIR"); old != "" {
		cfg.OldCodeSourceDir = old
		cfg.OldCodeNodes = gwPayFaultedNodes
		cfg.OldCodeSysconfig = true
		t.Logf("pre-fix binary from %s on nodes %v", old, gwPayFaultedNodes)
	}
	d, ctx := startDevnetNoKey(t, cfg, 110*time.Minute)
	t.Cleanup(func() {
		gwPayCompletionFault(t, d, context.Background(), gwPayFaultedNodes, false)
	})

	// ── preconditions: a committee, a gateway authority, funded payer.
	if err := d.waitForElectionEpoch(ctx, 1, 2, 20*time.Minute); err != nil {
		t.Fatalf("PRECONDITION FAILED: no epoch 2: %v", err)
	}
	for end := devnetNow().Add(20 * time.Minute); ; devnetSleep(15 * time.Second) {
		if _, w := gwActiveWeights(t, d); len(w) >= 8 {
			break
		}
		if devnetNow().After(end) {
			t.Fatalf("PRECONDITION FAILED: no gateway authority installed (authority %s)", gwAuthority(t, d))
		}
	}
	t.Logf("gateway authority: %s", gwAuthority(t, d))

	payer, payee := 1, 2
	payeeAcct := d.witnessAccount(payee)
	if _, err := d.Deposit(ctx, payer, "20.000", "hbd"); err != nil {
		t.Fatalf("PRECONDITION FAILED: deposit: %v", err)
	}
	for end := devnetNow().Add(5 * time.Minute); ; devnetSleep(5 * time.Second) {
		if b, err := d.GetAccountBalance(ctx, 1, "hive:"+d.witnessAccount(payer)); err == nil && b.Hbd >= 20_000 {
			break
		}
		if devnetNow().After(end) {
			t.Fatalf("PRECONDITION FAILED: the HBD deposit was never credited on L2")
		}
	}

	// payOnce issues a withdrawal, waits for the L1 credit, then keeps watching
	// for `settle` longer. It returns the credit actually received, so a SECOND
	// payment is visible as an overpayment rather than being hidden by a
	// ">= expected" wait.
	payOnce := func(label, amount string, milli int64, settle time.Duration) int64 {
		before := gwL1Hbd(t, d, payeeAcct)
		tx, err := d.Withdraw(payer, payeeAcct, amount, "hbd", label)
		if err != nil {
			t.Fatalf("%s: withdraw: %v", label, err)
		}
		start := devnetNow()
		paid := false
		for end := devnetNow().Add(15 * time.Minute); devnetNow().Before(end); devnetSleep(10 * time.Second) {
			if gwL1Hbd(t, d, payeeAcct) >= before+milli {
				paid = true
				t.Logf("%s: %s HBD reached %s after %s (withdraw tx %s)",
					label, amount, payeeAcct, devnetSince(start).Round(time.Second), tx)
				break
			}
		}
		if !paid {
			t.Errorf("RESULT %s: NOT paid within 15 min (tx %s, L1 delta %d)", label, tx, gwL1Hbd(t, d, payeeAcct)-before)
			return gwL1Hbd(t, d, payeeAcct) - before
		}
		// Watch past the next action ticks: a re-pay shows up here.
		for end := devnetNow().Add(settle); devnetNow().Before(end); devnetSleep(15 * time.Second) {
		}
		got := gwL1Hbd(t, d, payeeAcct) - before
		t.Logf("RESULT %s: L1 credit %d milli-HBD after %s of settling (want exactly %d)",
			label, got, settle, milli)
		return got
	}

	// ── P0 CONTROL: pay-once on a healthy fleet. Nothing asserts this today.
	if got := payOnce("GW-PAY0 control", "3.000", 3000, 3*time.Minute); got != 3000 {
		t.Errorf("CASE GW-PAY0 FAIL: healthy fleet credited %d milli-HBD for one 3.000 HBD withdrawal, want exactly 3000", got)
	} else {
		t.Logf("CASE GW-PAY0 PASS: paid exactly once on a healthy fleet")
	}

	// ── P1 FAULT: the settlement write fails on the signing majority.
	gwPayCompletionFault(t, d, ctx, gwPayFaultedNodes, true)
	gwPayVerifyFault(t, d, ctx, gwPayFaultedNodes[0], cfg.Nodes)

	got := payOnce("GW-PAY1 fault", "1.000", 1000, 5*time.Minute)

	// The breaker probe. last_processed_block is written by the streamer only
	// AFTER hive-consumer.ProcessBlock returns (streamer.go: processBlock calls
	// s.process synchronously, then updates lastSaved), so a node blocked in
	// blockingRetry inside the state engine freezes this height. Sample it
	// twice: a faulted node that has hit its settlement block does not advance.
	// Measured here rather than from before the payout, because the faulted
	// nodes keep processing normally right up to that block.
	frozen := map[int]uint64{}
	for _, n := range gwPayFaultedNodes {
		if bh, ok := nodeBlockOpt(d, ctx, n); ok {
			frozen[n] = bh
		}
	}
	devnetSleep(60 * time.Second)
	stalled := 0
	for _, n := range gwPayFaultedNodes {
		bh, ok := nodeBlockOpt(d, ctx, n)
		was, had := frozen[n]
		if ok && had && bh <= was {
			stalled++
		}
		t.Logf("  magi-%d processed height %d -> %d over 60s", n, was, bh)
	}

	if gwPayPreFix() {
		if got <= 1000 {
			t.Errorf("CASE GW-PAY1 UNPROVEN: the pre-fix binary credited %d, so the re-pay did not reproduce here", got)
		} else {
			t.Logf("CASE GW-PAY1 BUG REPRODUCED (pre-fix): credited %d milli-HBD for one 1.000 HBD withdrawal", got)
		}
	} else {
		if got != 1000 {
			t.Errorf("CASE GW-PAY1 FAIL: credited %d milli-HBD for one 1.000 HBD withdrawal while the settlement write failed on %d of %d nodes, want exactly 1000 — this is the double-pay",
				got, len(gwPayFaultedNodes), cfg.Nodes)
		} else {
			t.Logf("CASE GW-PAY1 PASS: paid exactly once with the settlement write failing on %d of %d nodes", len(gwPayFaultedNodes), cfg.Nodes)
		}
		if stalled == 0 {
			t.Errorf("CASE GW-PAY1 WEAK: no faulted node stopped advancing, so the breaker never engaged and pay-once here may be luck rather than the fix")
		} else {
			t.Logf("CASE GW-PAY1 breaker: %d of %d faulted nodes stopped processing blocks, as fail-stop requires", stalled, len(gwPayFaultedNodes))
		}
	}

	// ── P2 RECOVER: clearing the fault must unwedge every node and strand nothing.
	gwPayCompletionFault(t, d, ctx, gwPayFaultedNodes, false)
	resumed := 0
	for end := devnetNow().Add(10 * time.Minute); devnetNow().Before(end); devnetSleep(15 * time.Second) {
		resumed = 0
		for _, n := range gwPayFaultedNodes {
			if bh, ok := nodeBlockOpt(d, ctx, n); ok && bh > frozen[n]+2 {
				resumed++
			}
		}
		if resumed == len(gwPayFaultedNodes) {
			break
		}
	}
	if resumed != len(gwPayFaultedNodes) {
		t.Errorf("CASE GW-PAY2 FAIL: only %d of %d faulted nodes resumed processing after the fault cleared — fail-stop must be recoverable, not terminal",
			resumed, len(gwPayFaultedNodes))
	} else {
		t.Logf("CASE GW-PAY2 recover: all %d faulted nodes resumed", resumed)
	}

	// No action may be left pending (re-payable) or "processing" (the reverted
	// marker's stranding shape) on any node.
	strandOK := true
	for n := 1; n <= cfg.Nodes; n++ {
		counts := gwPayActionCounts(t, d, ctx, n)
		if counts["processing"] != 0 {
			strandOK = false
			t.Errorf("CASE GW-PAY2 FAIL: magi-%d has %d withdraw action(s) in \"processing\" — nothing may mark a withdrawal unavailable", n, counts["processing"])
		}
		if counts["pending"] != 0 {
			strandOK = false
			t.Errorf("CASE GW-PAY2 FAIL: magi-%d still has %d withdraw action(s) \"pending\" after recovery — a paid action left pending is re-paid every tick; an unpaid one is stranded", n, counts["pending"])
		}
		t.Logf("  magi-%d withdraw actions: %v", n, counts)
	}

	// ── P3 AFTER: the gateway still pays, exactly once.
	after := payOnce("GW-PAY2 after-recovery", "2.000", 2000, 3*time.Minute)
	if after != 2000 {
		t.Errorf("CASE GW-PAY2 FAIL: a withdrawal issued after recovery credited %d milli-HBD, want exactly 2000", after)
	} else if strandOK && resumed == len(gwPayFaultedNodes) {
		t.Logf("CASE GW-PAY2 PASS: nothing stranded, nothing re-payable, and the gateway pays normally after recovery")
	}

	t.Logf("RESULT GW-PAY totals: payee %s holds %d milli-HBD; expected credit was 3000 + 1000 + 2000",
		payeeAcct, gwL1Hbd(t, d, payeeAcct))
}
