package devnet

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"vsc-node/lib/dids"
	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// REWARD-ATTEST-1 on a real fleet.
//
// The block leader stops collecting signatures at dids.LeaderSigsComplete — 90%
// of the folded committee weight — and broadcasts. On a 5-seat flat-weight
// devnet committee that bar is 4, so one seat is missing from the Signers list
// of every healthy block, and which seat rotates with network latency. Below the
// 0.14.0 line each such absence cost 25 bps of the epoch's reward share, which
// is what charged every single mainnet seat in epochs 2221/2227/2229 with none
// at zero.
//
// Two arms, both real runs, differing only in the election version floor:
//
//	TestPendulumAttestQuorumDevnet        floor 0.14.0 — the fix is in force
//	TestPendulumAttestQuorumControlDevnet floor 0.13.0 — the fix is inert
//
// The CONTROL is what makes this a proof. It must show the live fingerprint —
// at least one settlement reducing EVERY committee member — and the fixed arm
// must show none. Without the control, "no reductions" is equally consistent
// with a devnet that never produced a block, never composed a settlement, or
// never reached the gated code at all; each of those is a way a security test
// passes while exercising nothing.
//
// Both arms also assert the consensus property the fix has to preserve: the
// reduction rows are byte-identical on all five nodes for the same epoch. A
// settlement that one node re-derives differently fails its BLS aggregate and
// stalls the election, so a divergence here would be worse than the bug.
//
// Run (one at a time — two devnets contend for disk and memory):
//
//	go test -v -run TestPendulumAttestQuorumDevnet        -timeout 60m ./tests/devnet/
//	go test -v -run TestPendulumAttestQuorumControlDevnet -timeout 60m ./tests/devnet/
func TestPendulumAttestQuorumDevnet(t *testing.T) {
	runAttestQuorumArm(t, 14, "0.14")
}

func TestPendulumAttestQuorumControlDevnet(t *testing.T) {
	runAttestQuorumArm(t, 13, "0.13")
}

// attestEpochReductions is one epoch's attested reward-reduction rows, keyed by
// account, as read back from a single node.
type attestEpochReductions struct {
	epoch uint64
	rows  map[string]int
}

func runAttestQuorumArm(t *testing.T, floorConsensus uint64, wantActiveLine string) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}

	cfg := DefaultConfig()
	cfg.LogLevel = "error,pendulum=trace"
	// ElectionInterval 800 so an epoch holds enough L2 blocks for a seat's
	// absences to survive PerEpochForgivenessBps (250, i.e. 11 absences at 25
	// bps each). At the 120 this test first used, a ~165-block epoch holds at
	// most ~16 L2 blocks (one per 10-block slot); with 5 seats and a bar of 4
	// each block omits exactly one seat, so a seat accrues ~3 absences and ~75
	// bps, forgiven every time however much traffic is generated. 800 gives
	// ~840 blocks per epoch, ~84 L2 blocks, ~16 absences per seat.
	//
	// ⚠ NECESSARY BUT NOT SUFFICIENT, and the reason the arms SKIP rather than
	// assert on an unfunded devnet. ComposeRecord short-circuits an
	// empty-activity epoch: with BucketBalanceHBD == 0 it emits a marker-only
	// record carrying NIL reductions, so reward_reductions is empty whatever the
	// scorer computed. A devnet credits pendulum:nodes:hbd only through a real
	// swap on a whitelisted pool -- see TestPendulumLPFloorDevnet, which deploys
	// an amm-pool contract, whitelists it, restarts the fleet, funds it and fires
	// a swap. Until this test does the same, BOTH arms read empty and neither
	// proves anything.
	//
	// (An earlier revision of this comment blamed forgiveness alone for the void
	// control run. That was wrong: forgiveness does bite, but it sits downstream
	// of a gate that was never reached.)
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorConsensus: floorConsensus,
			ElectionInterval:               800,
		},
	}

	// EVERY budget here is on the TEST CLOCK, which is denominated in BLOCKS:
	// devclock.go, "a budget of X covers at most about X/3s blocks". An earlier
	// run died on exactly that rather than on its subject -- a 30-minute wait is
	// 600 blocks, against an epoch that landed near block 862 -- so these are
	// sized in blocks, not in wall time.
	//
	// At ElectionInterval 800 an epoch costs ~840 blocks, so epoch 4 lands near
	// block 3,400: ~170 minutes of TEST clock, and roughly 25 minutes of wall
	// clock under FastBlocks. That exceeds vfDevnetBudgetCap's 75-minute default,
	// so the runner exports DEVNET_BUDGET_CAP; without it vfTestBudget truncates
	// to the cap (loudly) and the run strands mid-epoch.
	d, ctx := startDevnetNoKey(t, cfg, vfTestBudget(230*time.Minute))

	t.Log("waiting for the first running election on every node...")
	for n := 1; n <= cfg.Nodes; n++ {
		// 70 min of test clock = ~1,400 blocks; epoch 1 is the genesis election.
		nodeCtx, cancel := devnetWithTimeout(ctx, 70*time.Minute)
		err := d.waitForElectionEpoch(nodeCtx, n, 1, 70*time.Minute)
		cancel()
		if err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never ingested epoch >= 1: %v", n, err)
		}
	}

	// Wait for epoch 2 — the first election a node generates under this config,
	// and therefore the first one carrying the version floor. Checking the line
	// before it exists reads 0.0 on every arm.
	t.Log("waiting for epoch 2, the first election generated under this config...")
	for n := 1; n <= cfg.Nodes; n++ {
		nodeCtx, cancel := devnetWithTimeout(ctx, 70*time.Minute)
		err := d.waitForElectionEpoch(nodeCtx, n, 2, 70*time.Minute)
		cancel()
		if err != nil {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never ingested epoch >= 2: %v", n, err)
		}
	}

	// PRECONDITION 1 — the gate is where this arm says it is. Without this the
	// two arms could silently be the same run.
	activeLine, running, _, err := d.ConsensusInfo(ctx, 1)
	if err != nil {
		t.Fatalf("reading consensus info: %v", err)
	}
	t.Logf("active consensus line = %s (binary announces %s)", activeLine, running)
	if activeLine != wantActiveLine {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("active consensus line is %s, want %s — this arm would not exercise what it claims",
			activeLine, wantActiveLine)
	}

	// L2 traffic. canProduce only builds a block when there is something to put
	// in it, so without this the committee signs nothing and attestation scoring
	// has no input at all.
	if _, err := d.Deposit(ctx, 1, "20.000", "hbd"); err != nil {
		t.Fatalf("deposit hbd: %v", err)
	}
	if !waitForL2Balance(t, d, ctx, 1, witnessHive(d, 1), "hbd", 15_000, 5*time.Minute) {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("witness-1 HBD deposit never credited on L2")
	}

	var sent, failed int64
	stopTraffic := make(chan struct{})
	trafficDone := make(chan struct{})
	go func() {
		defer close(trafficDone)
		for {
			select {
			case <-stopTraffic:
				return
			case <-ctx.Done():
				return
			case <-devnetAfter(4 * time.Second):
				if _, err := d.Transfer(1, 2, "0.001", "hbd", "attest-quorum"); err != nil {
					atomic.AddInt64(&failed, 1)
					continue
				}
				atomic.AddInt64(&sent, 1)
			}
		}
	}()

	// Epoch 4, and the floor's activation timing is why it is not 2 or 3.
	//
	// The devnet's genesis-elector writes epoch 1 directly, and that election
	// carries NO version floor; the first election generated by a node under this
	// config is epoch 2. So the chain-active line only becomes `wantActiveLine`
	// at epoch 2's election, and the ticks INSIDE epoch 2 resolve against the
	// pre-activation floor (buildTickInputs resolves ActiveConsensusVersion at
	// the tick height). Epoch 2's settlement is therefore scored under the OLD
	// rule even in the gate arm, and only epoch 3 onwards is scored under the
	// gate. firstScoredEpoch encodes that; targeting 4 leaves two usable epochs.
	//
	// At ElectionInterval 120 this was invisible: epoch 2 landed within seconds
	// of epoch 1, so the line was already active when the arm checked it. At 800
	// it is ~12 minutes out, and the precondition below caught it -- an arm that
	// would have scored every epoch under the wrong rule and called it a pass.
	const targetEpoch = uint64(4)
	const firstScoredEpoch = uint64(3)
	t.Logf("generating L2 traffic and waiting for epoch >= %d on every node...", targetEpoch)
	for n := 1; n <= cfg.Nodes; n++ {
		// 200 min of test clock = ~4,000 blocks against the ~3,400 epoch 4 needs.
		nodeCtx, cancel := devnetWithTimeout(ctx, 200*time.Minute)
		err := d.waitForElectionEpoch(nodeCtx, n, targetEpoch, 200*time.Minute)
		cancel()
		if err != nil {
			close(stopTraffic)
			<-trafficDone
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("magi-%d never reached epoch %d: %v", n, targetEpoch, err)
		}
	}
	close(stopTraffic)
	<-trafficDone
	t.Logf("L2 traffic: %d transfers accepted, %d submission errors", atomic.LoadInt64(&sent), atomic.LoadInt64(&failed))

	committee, err := d.GetElectionGQL(ctx, 1, 2)
	if err != nil || committee == nil {
		t.Fatalf("reading the epoch-2 election: %v", err)
	}
	sort.Strings(committee.Members)
	t.Logf("committee (%d seats, total weight %d): %v",
		len(committee.Members), committee.TotalWeight, committee.Members)

	// PRECONDITION 2 — the fleet actually exhibits the leader bar. If every
	// block carries every seat's signature there is nothing for either arm to
	// disagree about, and a pass would mean nothing.
	bar, closedShort, blocksSeen := attestBlockShape(t, ctx, d, 1, len(committee.Members), committee.TotalWeight)
	t.Logf("blocks seen=%d, leader bar=%d of %d seats, blocks closed at/above the bar with a seat missing=%d",
		blocksSeen, bar, len(committee.Members), closedShort)
	if blocksSeen == 0 {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("no L2 blocks were produced — attestation scoring had no input, so this run proves nothing")
	}
	if closedShort == 0 {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("every block carried every seat's signature — the leader bar never left anyone out, so this run cannot tell the arms apart")
	}

	// Read the attested reduction rows for every settled epoch, on every node.
	var perEpoch []attestEpochReductions
	for epoch := firstScoredEpoch; epoch <= targetEpoch; epoch++ {
		var first map[string]int
		for n := 1; n <= cfg.Nodes; n++ {
			rows, bucket, ok, err := d.GetSettlementReductionsWithBucket(ctx, n, epoch)
			if err != nil {
				t.Fatalf("magi-%d reading settlement for epoch %d: %v", n, epoch, err)
			}
			if ok && bucket == 0 {
				// ComposeRecord's empty-activity short circuit: a zero bucket
				// yields a marker-only record with nil reductions, so there is
				// nothing to assert here -- on EITHER arm. Skipping is the honest
				// outcome; asserting "no reductions" would be a pass that proves
				// nothing, which is the failure mode this test exists to avoid.
				t.Skipf("epoch %d settled with bucket_balance_hbd=0, so ComposeRecord emitted a "+
					"marker-only record with nil reductions and reward_reductions is empty "+
					"regardless of the gate. This arm cannot prove anything until the fixture "+
					"credits the pendulum:nodes:hbd bucket with a real swap on a whitelisted "+
					"pool (recipe: TestPendulumLPFloorDevnet).", epoch)
			}
			if !ok {
				if n == 1 {
					t.Logf("epoch %d carries no settlement record (genesis transition); skipping", epoch)
				}
				first = nil
				break
			}
			m := make(map[string]int, len(rows))
			for _, r := range rows {
				m[r.Account] = r.Bps
			}
			if n == 1 {
				first = m
				continue
			}
			// The consensus property: every node must re-derive the same rows.
			if !sameReductionRows(first, m) {
				t.Fatalf("epoch %d: magi-1 and magi-%d disagree on the attested reductions\n  magi-1: %s\n  magi-%d: %s",
					epoch, n, formatReductions(first), n, formatReductions(m))
			}
		}
		if first == nil {
			continue
		}
		t.Logf("epoch %d reductions: %s", epoch, formatReductions(first))
		perEpoch = append(perEpoch, attestEpochReductions{epoch: epoch, rows: first})
	}
	if len(perEpoch) == 0 {
		dumpDiagnostics(t, d, ctx)
		t.Fatal("no settled epoch carried a settlement record — nothing was scored, so this run proves nothing")
	}

	// The fingerprint: an epoch in which EVERY seat is reduced. Under the bug it
	// is the inevitable outcome, because every seat is the slowest sometimes.
	// Under the fix it would need every seat to miss a block the leader waited
	// the full window for, which a healthy fleet does not do.
	fullSweeps := 0
	for _, e := range perEpoch {
		all := true
		for _, m := range committee.Members {
			if e.rows[attestAccountKey(m)] <= 0 && e.rows[m] <= 0 {
				all = false
				break
			}
		}
		if all {
			fullSweeps++
			t.Logf("epoch %d reduces EVERY one of the %d seats", e.epoch, len(committee.Members))
		}
	}

	if floorConsensus < 14 {
		if fullSweeps == 0 {
			t.Fatalf("CONTROL did not reproduce the bug: no epoch reduced all %d seats (epochs read: %d). "+
				"The fixed arm's result is therefore not evidence of anything.",
				len(committee.Members), len(perEpoch))
		}
		t.Logf("CONTROL reproduced REWARD-ATTEST-1: %d of %d settled epochs reduce every seat", fullSweeps, len(perEpoch))
		return
	}
	if fullSweeps != 0 {
		t.Fatalf("%d settled epoch(s) still reduce every one of the %d seats with 0.14.0 active — "+
			"the leader-bar skip is not taking effect on a real fleet", fullSweeps, len(committee.Members))
	}
	t.Logf("0.14.0 active: none of the %d settled epochs reduces every seat", len(perEpoch))
}

// attestAccountKey mirrors the "hive:" prefixing the settlement composer
// applies to committee accounts, so a row can be found either way.
func attestAccountKey(account string) string {
	return "hive:" + account
}

// attestBlockShape reads a node's stored block headers and reports the leader's
// signature bar, how many blocks closed at or above it while still missing a
// seat, and how many blocks were seen at all.
func attestBlockShape(t *testing.T, ctx context.Context, d *Devnet, node, seats int, totalWeight uint64) (bar, closedShort, seen int) {
	t.Helper()
	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)

	cur, err := client.Database(d.nodeDbName(node)).Collection("block_headers").
		Find(ctx, bson.M{}, options.Find().SetSort(bson.D{{Key: "slot_height", Value: 1}}))
	if err != nil {
		t.Fatalf("reading block_headers: %v", err)
	}
	defer cur.Close(ctx)

	// Flat POA weight: the folded total is the seat count, which is what the
	// leader compares 90% against.
	for s := 0; s <= seats; s++ {
		if dids.LeaderSigsComplete(uint64(s), uint64(seats), totalWeight) {
			bar = s
			break
		}
	}
	for cur.Next(ctx) {
		var rec struct {
			Signers []string `bson:"signers"`
		}
		if err := cur.Decode(&rec); err != nil {
			continue
		}
		seen++
		if len(rec.Signers) < seats && bar > 0 && len(rec.Signers) >= bar {
			closedShort++
		}
	}
	return bar, closedShort, seen
}

func sameReductionRows(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func formatReductions(m map[string]int) string {
	if len(m) == 0 {
		return "(none)"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := ""
	for i, k := range keys {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("%s=%d", k, m[k])
	}
	return out
}
