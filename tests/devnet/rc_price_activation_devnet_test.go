package devnet

// What a contract call is charged on a real network, before and after the floor
// reaches consensus 0.15.0: a new stored byte costs 19 RC before and 1 RC from
// the election that carries it, and a call that runs out is charged past its
// rc_limit before and exactly its limit after.
//
// One 5-node devnet starts on floor 0.14, measures every kind of charge, raises
// the floor to 0.15 with the on-chain proposal (the mainnet procedure), and
// measures the same calls again. What a call was charged is read from each
// node's own RC records.
//
//	go test -v -run TestRcPrice_Activation -timeout 150m ./tests/devnet/

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

const (
	// rcpSize is the value size the price is measured with. At 19 RC a byte it
	// costs 76,000 RC, under the 100,000 a dry run accepts.
	rcpSize = 4000
	// rcpFreeSize is what a free-tier account tries to store under rcpFreeLimit:
	// 300 new bytes cost 5,700 RC at 19 and 300 at 1.
	rcpFreeSize  = 300
	rcpFreeLimit = 5000
)

func TestRcPrice_Activation(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet write price test in short mode")
	}
	requireDocker(t)

	wasm := buildRcProbeContract(t)

	const nodes = 5
	cfg := DefaultConfig()
	cfg.Nodes = nodes
	cfg.LogLevel = "info"
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval:               20,
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 14,
		},
	}
	d, ctx := startDevnetNoKey(t, cfg, vfTestBudget(140*time.Minute))

	if err := d.WaitForBlockProcessing(ctx, 1, 30, 8*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}
	if err := d.waitForElectionEpoch(ctx, 1, 1, 10*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("first election never indexed: %v", err)
	}
	if err := d.waitForRcBackfill(ctx); err != nil {
		t.Fatalf("witness RC funding: %v", err)
	}

	cid, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: "rcprobe", DeployerNode: 1, GQLNode: 2})
	if err != nil {
		t.Fatalf("deploy the probe contract: %v", err)
	}
	t.Logf("probe contract %s", cid)
	if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
		t.Fatalf("nodes not synced after the deploy: %v", err)
	}
	rcpSettleAfterDeploy(t, d, ctx, nodes)

	// Payers. Each witness holds 990 HBD on the ledger (990,000 RC) plus the
	// free 10,000. initminer holds no HBD on the ledger: the free tier only.
	w := func(n int) string { return d.witnessAccount(n) }
	const freeTier = "initminer"
	payers := []string{"hive:" + w(1), "hive:" + w(2), "hive:" + w(3), "hive:" + w(4), "hive:" + w(5), "hive:" + freeTier}
	allNodes := []int{1, 2, 3, 4, 5}
	blob := func(key string, size int) string { return fmt.Sprintf("%s:%d", key, size) }
	hbdIntent := func(limit string) []map[string]any {
		return []map[string]any{{"type": "transfer.allow", "args": map[string]any{"limit": limit, "token": "hbd"}}}
	}

	// must runs a case every later case depends on.
	must := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("%s failed; the cases after it depend on it", name)
		}
	}

	var before struct {
		store, nested, rewrite, log, ret, transfer int64
	}

	// ------------------------------------------------------------------
	// Before: the chain runs consensus 0.14.
	// ------------------------------------------------------------------
	must("before/01_every_node_runs_0.15_code_on_an_active_0.14", func(t *testing.T) {
		if err := pollAllNodes(nodes, 8*time.Minute, func(n int) bool {
			line, running, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.14" && cvRunsAtLeast(running, 15)
		}); err != nil {
			line, running, _, _ := d.ConsensusInfo(ctx, 1)
			t.Fatalf("want active line 0.14 with nodes running 0.15 or later, got line=%q running=%q: %v", line, running, err)
		}
	})

	must("before/02_a_new_byte_costs_19", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(2), cid, "setBlob", blob("a1", rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		if lo, hi := int64(rcpSize*19), int64(rcpSize*19+400); o.Used < lo || o.Used > hi {
			t.Fatalf("storing %d new bytes cost %d RC, want %d to %d (19 a byte)", rcpSize, o.Used, lo, hi)
		}
		before.store = o.Used
	})

	t.Run("before/03_the_dry_run_reports_the_chains_price_on_every_node", func(t *testing.T) {
		for _, n := range allNodes {
			rc, ok, msg, err := rcpSim(ctx, d, n, w(2), cid, "setBlob", blob("a2", rcpSize), 100000)
			if err != nil || !ok {
				t.Errorf("magi-%d dry run: ok=%v %s %v", n, ok, msg, err)
				continue
			}
			if rc != before.store {
				t.Errorf("magi-%d dry run reports %d RC, the chain charged %d for the same call", n, rc, before.store)
			}
		}
	})

	must("before/04_a_nested_call_stores_at_19", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(3), cid, "relayBlob", cid+":"+blob("n1", rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		if lo, hi := int64(rcpSize*19), int64(rcpSize*19+600); o.Used < lo || o.Used > hi {
			t.Fatalf("a nested call storing %d new bytes cost %d RC, want %d to %d", rcpSize, o.Used, lo, hi)
		}
		before.nested = o.Used
	})

	must("before/05_a_rewrite_costs_the_read_price", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(4), cid, "setBlob", blob("a1", rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		// The key and the old value are read, the bytes put back are charged once more.
		if lo, hi := int64(2*rcpSize), int64(2*rcpSize+400); o.Used < lo || o.Used > hi {
			t.Fatalf("rewriting %d bytes cost %d RC, want %d to %d (about 2 a byte)", rcpSize, o.Used, lo, hi)
		}
		before.rewrite = o.Used
	})

	must("before/06_a_log_costs_the_read_price", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(5), cid, "logBlob", fmt.Sprint(rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		if lo, hi := int64(rcpSize), int64(rcpSize+400); o.Used < lo || o.Used > hi {
			t.Fatalf("logging %d bytes cost %d RC, want %d to %d (1 a byte)", rcpSize, o.Used, lo, hi)
		}
		before.log = o.Used
	})

	must("before/07_a_returned_value_is_not_charged_as_storage", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(5), cid, "retBlob", fmt.Sprint(rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		before.ret = o.Used
	})

	must("before/08_the_free_tier_cannot_pay_for_300_new_bytes", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, freeTier, cid, "setBlob", blob("f1", rcpFreeSize), rcpFreeLimit, nil)
		if o.Status != "FAILED" {
			t.Fatalf("storing %d new bytes under rc_limit %d: status %s, want FAILED (it costs %d at 19 a byte)", rcpFreeSize, rcpFreeLimit, o.Status, rcpFreeSize*19)
		}
		// Up to 0.14 a call that runs out is charged the whole storage
		// operation that took it past its limit.
		if o.Used < rcpFreeSize*19 || o.Used > rcpFreeSize*19+400 {
			t.Fatalf("the failed call was charged %d RC, want about %d (the whole write, past its limit %d)", o.Used, rcpFreeSize*19, rcpFreeLimit)
		}
	})

	must("before/08b_a_call_that_runs_out_is_charged_past_its_limit", func(t *testing.T) {
		// rc_limit 1,000 and one 200,000 byte log: the old rule charges the log in full.
		o := rcpMeasure(t, d, ctx, w(4), cid, "logBlob", "200000", 1000, nil)
		if o.Status != "FAILED" {
			t.Fatalf("status %s, want FAILED", o.Status)
		}
		if o.Used < 200000 {
			t.Fatalf("charged %d RC, want at least 200,000: this is the charge history was given", o.Used)
		}
	})

	must("before/09_a_ledger_transfer_costs_what_it_costs", func(t *testing.T) {
		payer := "hive:" + w(1)
		prev, havePrev, _ := rcpLatest(ctx, d, rcpRefNode, payer)
		if _, err := d.Transfer(1, 2, "0.001", "hbd", "rc price before"); err != nil {
			t.Fatalf("transfer: %v", err)
		}
		before.transfer = rcpWaitCharge(t, d, ctx, payer, prev, havePrev)
		t.Logf("a ledger transfer was charged %d RC", before.transfer)
	})

	t.Run("before/10_every_node_holds_the_same_rc_records", func(t *testing.T) {
		rcpNodesAgreeOnRc(t, d, ctx, allNodes, payers)
	})

	// ------------------------------------------------------------------
	// The switch: the on-chain proposal, as on mainnet.
	// ------------------------------------------------------------------
	var riseEpoch, riseHeight uint64
	must("switch/11_the_proposal_raises_the_floor_to_0.15", func(t *testing.T) {
		riseEpoch, riseHeight = rcpRaiseFloor(t, d, ctx, nodes, 15, 40*time.Minute)
		t.Logf("epoch %d at height %d carries 0.15", riseEpoch, riseHeight)
	})

	must("switch/12_every_node_reports_0.15_active", func(t *testing.T) {
		if err := pollAllNodes(nodes, 10*time.Minute, func(n int) bool {
			line, _, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.15"
		}); err != nil {
			t.Fatalf("not every node adopted 0.15: %v", err)
		}
	})

	t.Run("switch/13_a_wallet_reads_0.15_from_the_newest_election_on_every_node", func(t *testing.T) {
		for _, n := range allNodes {
			major, consensus, err := rcpActiveVersion(ctx, d, n)
			if err != nil || major != 0 || consensus != 15 {
				t.Errorf("magi-%d electionByBlockHeight: version %d.%d err=%v, want 0.15", n, major, consensus, err)
			}
		}
	})

	t.Run("switch/14_no_witness_fell_out_of_the_committee", func(t *testing.T) {
		members, err := d.GetElectionMembers(ctx, 1, riseEpoch)
		if err != nil {
			t.Fatalf("election %d members: %v", riseEpoch, err)
		}
		if len(members) != nodes {
			t.Fatalf("the election that carries 0.15 has %d members %v, want all %d witnesses", len(members), members, nodes)
		}
	})

	// ------------------------------------------------------------------
	// After: the same calls on consensus 0.15.
	// ------------------------------------------------------------------
	var after struct{ store int64 }
	must("after/15_a_new_byte_costs_1", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(2), cid, "setBlob", blob("b1", rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		// An RC record carries its slot's start height, so a call a few blocks
		// after the switch has a record up to one slot (10 blocks) below it.
		if o.Height+10 <= riseHeight {
			t.Fatalf("the call's RC record is at %d, a whole slot before the switch at %d", o.Height, riseHeight)
		}
		if lo, hi := int64(rcpSize), int64(rcpSize+400); o.Used < lo || o.Used > hi {
			t.Fatalf("storing %d new bytes cost %d RC, want %d to %d (1 a byte)", rcpSize, o.Used, lo, hi)
		}
		after.store = o.Used
	})

	t.Run("after/16_the_saving_is_exactly_18_rc_a_new_byte", func(t *testing.T) {
		if saved, want := before.store-after.store, int64(18*rcpSize); saved != want {
			t.Fatalf("the same call cost %d before and %d after: saved %d, want exactly %d", before.store, after.store, saved, want)
		}
	})

	t.Run("after/17_the_dry_run_reports_the_new_price_on_every_node", func(t *testing.T) {
		for _, n := range allNodes {
			rc, ok, msg, err := rcpSim(ctx, d, n, w(2), cid, "setBlob", blob("b2", rcpSize), 100000)
			if err != nil || !ok {
				t.Errorf("magi-%d dry run: ok=%v %s %v", n, ok, msg, err)
				continue
			}
			if rc != after.store {
				t.Errorf("magi-%d dry run reports %d RC, the chain charged %d for the same call", n, rc, after.store)
			}
		}
	})

	t.Run("after/18_a_nested_call_stores_at_1", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(3), cid, "relayBlob", cid+":"+blob("n2", rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		if saved, want := before.nested-o.Used, int64(18*rcpSize); saved != want {
			t.Fatalf("the nested call cost %d before and %d after: saved %d, want exactly %d", before.nested, o.Used, saved, want)
		}
	})

	t.Run("after/19_a_rewrite_costs_what_it_did", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(4), cid, "setBlob", blob("a1", rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" || o.Used != before.rewrite {
			t.Fatalf("status %s, charged %d RC; before the switch %d", o.Status, o.Used, before.rewrite)
		}
	})

	t.Run("after/20_a_log_costs_what_it_did", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(5), cid, "logBlob", fmt.Sprint(rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" || o.Used != before.log {
			t.Fatalf("status %s, charged %d RC; before the switch %d", o.Status, o.Used, before.log)
		}
	})

	t.Run("after/21_a_returned_value_costs_what_it_did", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(5), cid, "retBlob", fmt.Sprint(rcpSize), 500000, nil)
		if o.Status != "CONFIRMED" || o.Used != before.ret {
			t.Fatalf("status %s, charged %d RC; before the switch %d", o.Status, o.Used, before.ret)
		}
	})

	t.Run("after/22_the_free_tier_pays_for_the_same_300_bytes_under_the_same_limit", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, freeTier, cid, "setBlob", blob("f2", rcpFreeSize), rcpFreeLimit, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s, want CONFIRMED: %d new bytes cost about %d RC at 1 a byte", o.Status, rcpFreeSize, rcpFreeSize)
		}
		if o.Used < rcpFreeSize || o.Used > rcpFreeSize+400 {
			t.Fatalf("charged %d RC, want %d to %d", o.Used, rcpFreeSize, rcpFreeSize+400)
		}
	})

	t.Run("after/23_the_smallest_call_still_costs_100", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(2), cid, "noop", "x", 500000, nil)
		if o.Status != "CONFIRMED" || o.Used != 100 {
			t.Fatalf("status %s, charged %d RC, want the 100 RC minimum", o.Status, o.Used)
		}
	})

	t.Run("after/24_a_call_that_runs_out_is_charged_its_limit", func(t *testing.T) {
		const limit = 2000 // the call needs about 4,000
		o := rcpMeasure(t, d, ctx, w(4), cid, "setBlob", blob("g1", rcpSize), limit, nil)
		if o.Status != "FAILED" {
			t.Fatalf("status %s, want FAILED under rc_limit %d", o.Status, limit)
		}
		if o.Used != limit {
			t.Fatalf("the failed call was charged %d RC, want exactly its limit %d", o.Used, limit)
		}
		// Nothing it wrote may remain.
		st, err := d.GetStateByKeys(ctx, 3, cid, []string{"g1"})
		if err != nil {
			t.Fatalf("reading state: %v", err)
		}
		if v := st["g1"]; v != nil && fmt.Sprintf("%v", v) != "" {
			t.Fatalf("the failed call's write survived (%d bytes)", len(fmt.Sprintf("%v", v)))
		}
	})

	t.Run("after/24b_one_huge_log_under_a_small_limit_costs_the_limit", func(t *testing.T) {
		// The same call that cost over 200,000 RC before the switch.
		o := rcpMeasure(t, d, ctx, w(4), cid, "logBlob", "200000", 1000, nil)
		if o.Status != "FAILED" || o.Used != 1000 {
			t.Fatalf("status %s, charged %d RC, want FAILED and exactly the limit 1000", o.Status, o.Used)
		}
	})

	t.Run("after/25_a_limit_equal_to_the_cost_passes_and_one_less_fails", func(t *testing.T) {
		exact := rcpMeasure(t, d, ctx, w(2), cid, "setBlob", blob("h1", rcpSize), int(after.store), nil)
		if exact.Status != "CONFIRMED" || exact.Used != after.store {
			t.Fatalf("rc_limit %d (the cost): status %s, charged %d", after.store, exact.Status, exact.Used)
		}
		short := rcpMeasure(t, d, ctx, w(3), cid, "setBlob", blob("h2", rcpSize), int(after.store-1), nil)
		if short.Status != "FAILED" || short.Used != after.store-1 {
			t.Fatalf("rc_limit %d (one under the cost): status %s, charged %d, want FAILED and charged the limit", after.store-1, short.Status, short.Used)
		}
	})

	t.Run("after/26_a_ledger_transfer_costs_what_it_did", func(t *testing.T) {
		payer := "hive:" + w(1)
		prev, havePrev, _ := rcpLatest(ctx, d, rcpRefNode, payer)
		if _, err := d.Transfer(1, 2, "0.001", "hbd", "rc price after"); err != nil {
			t.Fatalf("transfer: %v", err)
		}
		if got := rcpWaitCharge(t, d, ctx, payer, prev, havePrev); got != before.transfer {
			t.Fatalf("a ledger transfer was charged %d RC, before the switch %d", got, before.transfer)
		}
	})

	t.Run("after/27_an_hbd_draw_under_a_small_limit_passes", func(t *testing.T) {
		// rc_limit at or under the free RC reserves none of the caller's HBD.
		o := rcpMeasure(t, d, ctx, w(5), cid, "drawHbd", "100000", 5000, hbdIntent("100.000"))
		if o.Status != "CONFIRMED" {
			t.Fatalf("drawing 100 HBD under rc_limit 5000: status %s", o.Status)
		}
		// Balances are folded at the next slot boundary.
		if !waitForL2Balance(t, d, ctx, 2, "contract:"+cid, "hbd", 100000, 4*time.Minute) {
			bal, err := d.GetAccountBalance(ctx, 2, "contract:"+cid)
			t.Fatalf("the contract holds %+v after the draw (err %v), want at least 100.000 HBD", bal, err)
		}
	})

	t.Run("after/28_an_hbd_draw_cannot_take_what_a_large_limit_reserves", func(t *testing.T) {
		// The node reserves (rc_limit - free RC left) of the caller's HBD before
		// the draw: with rc_limit 800,000 it reserves about 800 of the 890 HBD
		// the caller has left, so a 300 HBD draw must fail.
		o := rcpMeasure(t, d, ctx, w(5), cid, "drawHbd", "300000", 800000, hbdIntent("300.000"))
		if o.Status != "FAILED" {
			t.Fatalf("drawing 300 HBD under rc_limit 800000: status %s, want FAILED", o.Status)
		}
	})

	t.Run("after/29_every_node_holds_the_same_rc_records", func(t *testing.T) {
		rcpNodesAgreeOnRc(t, d, ctx, allNodes, payers)
	})

	t.Run("after/30_every_node_holds_the_same_contract_state", func(t *testing.T) {
		head := nodeBlock(t, d, ctx, 1)
		var wantId, wantRoot string
		for i, n := range allNodes {
			if err := d.WaitForBlockProcessing(ctx, n, head, 5*time.Minute); err != nil {
				t.Fatalf("magi-%d did not reach block %d: %v", n, head, err)
			}
			id, root, _, err := rcpLatestOutput(ctx, d, n, cid)
			if err != nil {
				t.Fatalf("magi-%d: %v", n, err)
			}
			if i == 0 {
				wantId, wantRoot = id, root
				continue
			}
			if id != wantId || root != wantRoot {
				t.Errorf("magi-%d newest output %s root %s, magi-1 has %s root %s", n, id, root, wantId, wantRoot)
			}
		}
	})

	// ------------------------------------------------------------------
	// Replay: a node rebuilds everything from genesis, across the switch.
	// ------------------------------------------------------------------
	t.Run("replay/31_a_node_replaying_from_genesis_rebuilds_the_same_rc_records", func(t *testing.T) {
		const node = 5
		if err := d.StopNode(ctx, node); err != nil {
			t.Fatalf("stopping magi-%d: %v", node, err)
		}
		// Tag its head as processed under 0.14: on restart the version-lag check
		// sees the chain on 0.15 and replays from genesis.
		if err := d.seedProcessedUnderVersion(ctx, node, 0, 14); err != nil {
			t.Fatalf("tagging magi-%d as processed under 0.14: %v", node, err)
		}
		if err := d.StartNode(ctx, node); err != nil {
			t.Fatalf("restarting magi-%d: %v", node, err)
		}
		if err := pollNode(node, 8*time.Minute, func() bool {
			logs, e := d.Logs(ctx, fmt.Sprintf("magi-%d", node))
			return e == nil && strings.Contains(logs, "consensus-version lag")
		}); err != nil {
			t.Fatalf("magi-%d did not start a version-lag replay: %v", node, err)
		}
		head := nodeBlock(t, d, ctx, 1)
		if err := d.WaitForBlockProcessing(ctx, node, head, 25*time.Minute); err != nil {
			t.Fatalf("magi-%d did not replay to block %d: %v", node, head, err)
		}
		rcpNodesAgreeOnRc(t, d, ctx, []int{rcpRefNode, node}, payers)
		id1, root1, _, err1 := rcpLatestOutput(ctx, d, rcpRefNode, cid)
		id5, root5, _, err5 := rcpLatestOutput(ctx, d, node, cid)
		if err1 != nil || err5 != nil || id1 != id5 || root1 != root5 {
			t.Fatalf("after the replay magi-%d has output %s root %s (err %v), magi-%d has %s root %s (err %v)", node, id5, root5, err5, rcpRefNode, id1, root1, err1)
		}
	})

	t.Run("replay/32_the_chain_keeps_advancing_on_every_node", func(t *testing.T) {
		head := nodeBlock(t, d, ctx, 1)
		for _, n := range allNodes {
			if err := d.WaitForBlockProcessing(ctx, n, head+10, 6*time.Minute); err != nil {
				dumpDiagnostics(t, d, ctx)
				t.Fatalf("magi-%d stalled: %v", n, err)
			}
		}
	})
}

// rcpWaitCharge waits for an account's next RC record on the reference node and
// returns what was charged since the given one.
func rcpWaitCharge(t *testing.T, d *Devnet, ctx context.Context, payer string, prev rcpRec, havePrev bool) int64 {
	t.Helper()
	deadline := devnetNow().Add(5 * time.Minute)
	for devnetNow().Before(deadline) {
		cur, ok, err := rcpLatest(ctx, d, rcpRefNode, payer)
		if err == nil && ok && (!havePrev || cur.Height > prev.Height) {
			return rcpCharge(prev, havePrev, cur)
		}
		devnetSleep(2 * time.Second)
	}
	t.Fatalf("no new RC record for %s", payer)
	return 0
}
