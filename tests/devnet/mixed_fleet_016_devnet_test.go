package devnet

// Two releases in one fleet while consensus 0.15.0 is active, on a real
// devnet: three nodes run this tree (it announces 0.16) and two run the
// release before it (0.15, MIXED_015_CODE_DIR). Every kind of call the 0.16.0
// rules treat differently is sent while 0.15 is active; both releases must
// sign each other's blocks and every node must hold the same results. Then the
// two are upgraded, the floor goes to 0.16, and the same calls are refused.
//
//	MIXED_015_CODE_DIR=/path/to/go-vsc-node-on-0.15 \
//	  go test -v -run TestHardening_MixedFleetAt015ThenActivates016 -timeout 200m ./tests/devnet/

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestHardening_MixedFleetAt015ThenActivates016(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet hardening test in short mode")
	}
	old := os.Getenv("MIXED_015_CODE_DIR")
	if old == "" {
		t.Skip("MIXED_015_CODE_DIR (a go-vsc-node tree on consensus 0.15) required")
	}
	requireDocker(t)
	wasm := buildRcProbeContract(t)

	const nodes = 5
	all := []int{1, 2, 3, 4, 5}
	oldCode := []int{4, 5}
	isOld := func(n int) bool { return n == 4 || n == 5 }

	cfg := DefaultConfig()
	cfg.Nodes = nodes
	cfg.LogLevel = "info"
	cfg.OldCodeSourceDir = old
	cfg.OldCodeNodes = append([]int(nil), oldCode...)
	cfg.OldCodeSysconfig = true
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval:               20,
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 15,
		},
	}
	d, ctx := startDevnetNoKey(t, cfg, vfTestBudget(190*time.Minute))

	if err := d.WaitForBlockProcessing(ctx, 1, 30, 10*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("network never reached block 30: %v", err)
	}
	if err := d.waitForElectionEpoch(ctx, 1, 1, 12*time.Minute); err != nil {
		dumpDiagnostics(t, d, ctx)
		t.Fatalf("first election never indexed: %v", err)
	}
	if err := d.waitForRcBackfill(ctx); err != nil {
		t.Fatalf("witness RC funding: %v", err)
	}
	for _, n := range all {
		if _, err := d.Deposit(ctx, n, "90.000", "hbd"); err != nil {
			t.Fatalf("deposit for %s: %v", d.witnessAccount(n), err)
		}
	}
	for _, n := range all {
		if !waitForL2Balance(t, d, ctx, 1, "hive:"+d.witnessAccount(n), "hbd", 1_080_000, 5*time.Minute) {
			t.Fatalf("%s never held 1,080 HBD on the ledger", d.witnessAccount(n))
		}
	}
	cid, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: "rcprobe", DeployerNode: 1, GQLNode: 2})
	if err != nil {
		t.Fatalf("deploy the probe contract: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
		t.Fatalf("nodes not synced after the deploy: %v", err)
	}
	rcpSettleAfterDeploy(t, d, ctx, nodes)

	w := func(n int) string { return d.witnessAccount(n) }
	payers := make([]string, 0, nodes)
	for _, n := range all {
		payers = append(payers, "hive:"+w(n))
	}
	must := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("%s failed; the cases after it depend on it", name)
		}
	}
	allow := []map[string]any{{"type": "transfer.allow", "args": map[string]string{"token": "hbd", "limit": "10.000"}}}
	bigValue := fmt.Sprintf("big:%d", params.MAX_STATE_VALUE_BYTES+1)
	bigKey := strings.Repeat("k", params.MAX_STATE_KEY_BYTES+1) + ",x"

	// ------------------------------------------------------------------
	// 0.15 active, two releases.
	// ------------------------------------------------------------------
	must("before/01_three_nodes_run_0.16_code_and_two_0.15_code_on_an_active_0.15", func(t *testing.T) {
		if err := pollAllNodes(nodes, 10*time.Minute, func(n int) bool {
			line, running, _, e := d.ConsensusInfo(ctx, n)
			if e != nil || line != "0.15" {
				return false
			}
			if isOld(n) {
				return strings.HasPrefix(running, "0.15")
			}
			return cvRunsAtLeast(running, 16)
		}); err != nil {
			for _, n := range all {
				line, running, _, e := d.ConsensusInfo(ctx, n)
				t.Logf("magi-%d: line=%q running=%q err=%v", n, line, running, e)
			}
			t.Fatalf("the fleet is not 3 x 0.16 code + 2 x 0.15 code on an active 0.15: %v", err)
		}
	})

	must("before/02_every_witness_is_seated", func(t *testing.T) {
		if err := pollNode(1, 20*time.Minute, func() bool {
			last, _, err := rhLatestElection(ctx, d, 1)
			if err != nil {
				return false
			}
			members, err := d.GetElectionMembers(ctx, 1, last)
			return err == nil && len(members) == nodes
		}); err != nil {
			last, _, _ := rhLatestElection(ctx, d, 1)
			members, _ := d.GetElectionMembers(ctx, 1, last)
			t.Fatalf("election %d seats %v, want all %d witnesses: %v", last, members, nodes, err)
		}
	})

	fromBlock := nodeBlock(t, d, ctx, 1)
	var txs015 []string

	must("at015/03_a_key_over_1_KiB_is_stored", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(4), cid, "setString", bigKey, 100_000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("a key of 1 KiB + 1 at 0.15: status %s (%s)", o.Status, hardeningTxError(ctx, d, 1, o.TxId))
		}
		txs015 = append(txs015, o.TxId)
	})

	must("at015/04_two_600_KB_returns_in_one_block_both_pass", func(t *testing.T) {
		txId, err := hardeningCallsInOneBlock(d, []int{1, 2}, cid, "retBlob", "600000", 100_000)
		if err != nil {
			t.Fatalf("broadcasting the two calls: %v", err)
		}
		if st := rcpWaitFinal(d, ctx, 1, txId, 8*time.Minute); st != "CONFIRMED" {
			t.Fatalf("two 600 KB returns in one block at 0.15: %s (%s)", st, hardeningTxError(ctx, d, 1, txId))
		}
		txs015 = append(txs015, txId)
	})

	must("at015/05_5000_ledger_transfers_in_one_call_pass", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(1), cid, "manyTransfers", "5000", 100_000, allow)
		if o.Status != "CONFIRMED" {
			t.Fatalf("5,000 transfers at 0.15: status %s (%s)", o.Status, hardeningTxError(ctx, d, 1, o.TxId))
		}
		txs015 = append(txs015, o.TxId)
	})

	must("at015/06_a_value_over_1_MiB_is_stored", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(3), cid, "setBlob", bigValue, 1_080_000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("a value of 1 MiB + 1 at 0.15: status %s (%s)", o.Status, hardeningTxError(ctx, d, 1, o.TxId))
		}
		txs015 = append(txs015, o.TxId)
	})

	must("at015/07_a_call_that_runs_out_fails_and_is_charged_its_limit", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(5), cid, "logBlob", "200000", 1000, nil)
		if o.Status != "FAILED" || o.Used != 1000 {
			t.Fatalf("a 200,000-byte log under rc_limit 1,000 at 0.15: status %s, charged %d, want FAILED and 1,000", o.Status, o.Used)
		}
		txs015 = append(txs015, o.TxId)
	})

	must("at015/08_each_release_signs_blocks_the_other_proposed", func(t *testing.T) {
		mf16CrossSigned(t, d, ctx, cid, fromBlock, all, isOld, w(5))
	})

	must("at015/09_every_node_holds_the_same_results", func(t *testing.T) {
		mf16NodesAgree(t, d, ctx, cid, all, payers, txs015)
	})

	// ------------------------------------------------------------------
	// Upgrade the two, then 0.16.
	// ------------------------------------------------------------------
	must("upgrade/10_the_two_0.15_nodes_move_to_the_0.16_code_and_keep_up", func(t *testing.T) {
		for _, n := range oldCode {
			if err := d.rhUpgradeNode(ctx, n); err != nil {
				t.Fatalf("upgrading magi-%d: %v", n, err)
			}
			devnetSleep(60 * time.Second) // a node back on its peer ID is deaf on pubsub for about a minute
		}
		if err := pollAllNodes(nodes, 10*time.Minute, func(n int) bool {
			line, running, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.15" && cvRunsAtLeast(running, 16)
		}); err != nil {
			t.Fatalf("not every node runs the 0.16 code on an active 0.15: %v", err)
		}
		head := nodeBlock(t, d, ctx, 1)
		for _, n := range oldCode {
			if err := d.WaitForBlockProcessing(ctx, n, head+10, 15*time.Minute); err != nil {
				t.Fatalf("magi-%d did not keep up after the upgrade: %v", n, err)
			}
			if logs, e := d.Logs(ctx, fmt.Sprintf("magi-%d", n)); e == nil && strings.Contains(logs, "consensus-version lag") {
				t.Logf("magi-%d started a version-lag replay after the upgrade", n)
			}
		}
		mf16NodesAgree(t, d, ctx, cid, all, payers, txs015)
	})

	must("switch/11_the_floor_goes_to_0.16_and_every_witness_stays_seated", func(t *testing.T) {
		epoch, _ := rcpRaiseFloor(t, d, ctx, nodes, 16, 60*time.Minute)
		if err := pollAllNodes(nodes, 10*time.Minute, func(n int) bool {
			line, _, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.16"
		}); err != nil {
			t.Fatalf("not every node reports 0.16 active: %v", err)
		}
		members, err := d.GetElectionMembers(ctx, 1, epoch)
		if err != nil {
			t.Fatalf("election %d members: %v", epoch, err)
		}
		if len(members) != nodes {
			t.Fatalf("election %d (the first on 0.16) seats %v, want all %d witnesses", epoch, members, nodes)
		}
	})

	// ------------------------------------------------------------------
	// 0.16 active: the same calls are refused, and every node agrees.
	// ------------------------------------------------------------------
	var txs016 []string
	refused := func(t *testing.T, txId, what, want string) {
		t.Helper()
		reason := hardeningTxError(ctx, d, 1, txId)
		if !strings.Contains(reason, want) {
			t.Fatalf("%s failed for %q, want %q", what, reason, want)
		}
		t.Logf("%s: FAILED: %s", what, reason)
		txs016 = append(txs016, txId)
	}

	t.Run("at016/12_a_key_over_1_KiB_fails", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(2), cid, "setString", bigKey, 100_000, nil)
		if o.Status != "FAILED" {
			t.Fatalf("a key of 1 KiB + 1 at 0.16: status %s, want FAILED", o.Status)
		}
		refused(t, o.TxId, "a key of 1 KiB + 1", "over the 1024-byte limit")
	})

	t.Run("at016/13_two_600_KB_returns_in_one_block_fail", func(t *testing.T) {
		txId, err := hardeningCallsInOneBlock(d, []int{1, 2}, cid, "retBlob", "600000", 100_000)
		if err != nil {
			t.Fatalf("broadcasting the two calls: %v", err)
		}
		if st := rcpWaitFinal(d, ctx, 1, txId, 8*time.Minute); st != "FAILED" {
			t.Fatalf("two 600 KB returns in one block at 0.16: %s, want FAILED", st)
		}
		refused(t, txId, "two 600 KB returns in one block", "over the")
	})

	t.Run("at016/14_5000_ledger_transfers_in_one_call_fail", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(1), cid, "manyTransfers", "5000", 100_000, allow)
		if o.Status != "FAILED" {
			t.Fatalf("5,000 transfers at 0.16: status %s, want FAILED", o.Status)
		}
		refused(t, o.TxId, "5,000 transfers", "over the 1048576-byte limit")
	})

	t.Run("at016/15_a_value_over_1_MiB_fails", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(4), cid, "setBlob", bigValue, 1_080_000, nil)
		if o.Status != "FAILED" {
			t.Fatalf("a value of 1 MiB + 1 at 0.16: status %s, want FAILED", o.Status)
		}
		refused(t, o.TxId, "a value of 1 MiB + 1", "over the 1048576-byte limit")
	})

	t.Run("at016/16_every_node_holds_the_same_results_and_the_chain_advances", func(t *testing.T) {
		mf16NodesAgree(t, d, ctx, cid, all, payers, append(append([]string(nil), txs015...), txs016...))
		head := nodeBlock(t, d, ctx, 1)
		for _, n := range all {
			if err := d.WaitForBlockProcessing(ctx, n, head+20, 8*time.Minute); err != nil {
				dumpDiagnostics(t, d, ctx)
				t.Fatalf("magi-%d stalled at 0.16: %v", n, err)
			}
		}
	})
}

// mf16CrossSigned waits until every witness on one release has signed a block
// proposed by a witness on the other release, counting blocks from slot
// fromBlock on node 1. A cheap call is sent each slot from `filler` so blocks
// keep coming. A node signs only a block it computed the same way.
func mf16CrossSigned(t *testing.T, d *Devnet, ctx context.Context, cid string, fromBlock uint64, all []int, isOld func(int) bool, filler string) {
	t.Helper()
	group := map[string]bool{} // witness account -> runs the 0.15 code
	for _, n := range all {
		group[d.witnessAccount(n)] = isOld(n)
	}
	var pending []string
	deadline := devnetNow().Add(25 * time.Minute)
	for {
		blocks, signedOther, sample, err := mf16Signatures(ctx, d, 1, fromBlock, group)
		if err == nil {
			missing := []string{}
			for acct := range group {
				if signedOther[acct] == 0 {
					missing = append(missing, acct)
				}
			}
			sort.Strings(missing)
			if len(missing) == 0 {
				for _, n := range all {
					a := d.witnessAccount(n)
					t.Logf("%s (%s code) signed %d blocks the other release proposed", a, map[bool]string{true: "0.15", false: "0.16"}[isOld(n)], signedOther[a])
				}
				t.Logf("%d blocks from slot %d", blocks, fromBlock)
				for _, tx := range pending {
					rcpWaitFinal(d, ctx, 1, tx, 4*time.Minute)
				}
				return
			}
			if devnetNow().After(deadline) {
				t.Fatalf("after %d blocks, %v never signed a block the other release proposed (newest header: %s)", blocks, missing, sample)
			}
		} else if devnetNow().After(deadline) {
			t.Fatalf("reading block headers: %v", err)
		}
		pending = append(pending, rcpCallNoWait(t, d, filler, cid, "noop", "x", 1000))
		devnetSleep(30 * time.Second)
	}
}

// mf16Signatures counts, from slot fromBlock on one node, the blocks and, per
// witness, how many blocks it signed that a witness of the other release
// proposed.
func mf16Signatures(ctx context.Context, d *Devnet, node int, fromBlock uint64, group map[string]bool) (blocks int, signedOther map[string]int, sample string, err error) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, nil, "", err
	}
	defer client.Disconnect(ctx)
	cur, err := client.Database(d.nodeDbName(node)).Collection("block_headers").
		Find(ctx, bson.M{"slot_height": bson.M{"$gte": int64(fromBlock)}}, options.Find().SetSort(bson.M{"slot_height": 1}))
	if err != nil {
		return 0, nil, "", err
	}
	defer cur.Close(ctx)
	signedOther = map[string]int{}
	for cur.Next(ctx) {
		var h struct {
			Proposer string   `bson:"proposer"`
			Signers  []string `bson:"signers"`
		}
		if err := cur.Decode(&h); err != nil {
			continue
		}
		blocks++
		sample = fmt.Sprintf("proposer %q signers %q", h.Proposer, h.Signers)
		propOld, known := group[h.Proposer]
		if !known {
			continue
		}
		for _, s := range h.Signers {
			if sOld, ok := group[s]; ok && sOld != propOld {
				signedOther[s]++
			}
		}
	}
	return blocks, signedOther, sample, cur.Err()
}

// mf16NodesAgree checks that every node holds the same status for each
// transaction, the same RC records for each payer and the same newest output
// (id and state root) for the contract.
func mf16NodesAgree(t *testing.T, d *Devnet, ctx context.Context, cid string, nodes []int, payers []string, txIds []string) {
	t.Helper()
	rcpNodesAgreeOnRc(t, d, ctx, nodes, payers)
	for _, tx := range txIds {
		var want string
		for i, n := range nodes {
			st := rcpWaitFinal(d, ctx, n, tx, 3*time.Minute)
			if i == 0 {
				want = st
				continue
			}
			if st != want {
				t.Errorf("tx %s: magi-%d says %q, magi-%d says %q", tx, n, st, nodes[0], want)
			}
		}
	}
	var wantId, wantRoot string
	for i, n := range nodes {
		id, root, _, err := rcpLatestOutput(ctx, d, n, cid)
		if err != nil {
			t.Fatalf("magi-%d: %v", n, err)
		}
		if i == 0 {
			wantId, wantRoot = id, root
			continue
		}
		if id != wantId || root != wantRoot {
			t.Errorf("magi-%d newest output %s root %s, magi-%d has %s root %s", n, id, root, nodes[0], wantId, wantRoot)
		}
	}
	if !t.Failed() {
		t.Logf("%d transactions, %d payers and the newest contract output are the same on nodes %v", len(txIds), len(payers), nodes)
	}
}
