package devnet

// A node left on the old binary when the floor reaches consensus 0.15.0.
//
// Four of five nodes run this tree; the fifth runs the release before it
// (0.14, RC_PRICE_OLD_CODE_DIR). The four raise the floor. The old node is left
// out of the committee, keeps charging 19 RC a new byte in its own execution,
// and after it is upgraded it replays and holds the records of the others.
//
//	RC_PRICE_OLD_CODE_DIR=/path/to/go-vsc-node-at-0.14 \
//	  go test -v -run TestRcPrice_MixedFleet -timeout 200m ./tests/devnet/

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

func TestRcPrice_MixedFleet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet write price test in short mode")
	}
	old := os.Getenv("RC_PRICE_OLD_CODE_DIR")
	if old == "" {
		t.Skip("RC_PRICE_OLD_CODE_DIR (a go-vsc-node tree on consensus 0.14) required")
	}
	requireDocker(t)

	wasm := buildRcProbeContract(t)

	const nodes, oldNode = 5, 5
	cfg := DefaultConfig()
	cfg.Nodes = nodes
	cfg.LogLevel = "info"
	cfg.OldCodeSourceDir = old
	cfg.OldCodeNodes = []int{oldNode}
	cfg.OldCodeSysconfig = true
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval:               20,
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 14,
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
	cid, err := d.DeployContract(ctx, ContractDeployOpts{WasmPath: wasm, Name: "rcprobe", DeployerNode: 1, GQLNode: 2})
	if err != nil {
		t.Fatalf("deploy the probe contract: %v", err)
	}
	if err := d.WaitForBlockProcessing(ctx, 2, nodeBlock(t, d, ctx, 1), 3*time.Minute); err != nil {
		t.Fatalf("nodes not synced after the deploy: %v", err)
	}

	rcpSettleAfterDeploy(t, d, ctx, nodes)

	w := func(n int) string { return d.witnessAccount(n) }
	payer := "hive:" + w(2)
	newNodes := []int{1, 2, 3, 4}
	blob := func(key string) string { return fmt.Sprintf("%s:%d", key, rcpSize) }
	must := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			dumpDiagnostics(t, d, ctx)
			t.Fatalf("%s failed; the cases after it depend on it", name)
		}
	}

	var beforeUsed int64
	must("before/01_the_old_binary_runs_0.14_and_the_others_0.15", func(t *testing.T) {
		if err := pollAllNodes(nodes, 10*time.Minute, func(n int) bool {
			line, running, _, e := d.ConsensusInfo(ctx, n)
			if e != nil || line != "0.14" {
				return false
			}
			if n == oldNode {
				return strings.HasPrefix(running, "0.14")
			}
			return cvRunsAtLeast(running, 15)
		}); err != nil {
			for n := 1; n <= nodes; n++ {
				line, running, _, e := d.ConsensusInfo(ctx, n)
				t.Logf("magi-%d: line=%q running=%q err=%v", n, line, running, e)
			}
			t.Fatalf("the fleet is not 4 x 0.15 + 1 x 0.14 on an active 0.14: %v", err)
		}
	})

	must("before/02_all_five_charge_19_and_agree", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(2), cid, "setBlob", blob("a1"), 500000, nil)
		if o.Status != "CONFIRMED" || o.Used < int64(rcpSize*19) || o.Used > int64(rcpSize*19+400) {
			t.Fatalf("status %s, charged %d RC, want about %d", o.Status, o.Used, rcpSize*19)
		}
		beforeUsed = o.Used
		rcpNodesAgreeOnRc(t, d, ctx, []int{1, 2, 3, 4, 5}, []string{payer})
	})

	var riseEpoch uint64
	must("switch/03_four_of_five_raise_the_floor", func(t *testing.T) {
		riseEpoch, _ = rcpRaiseFloor(t, d, ctx, len(newNodes), 15, 60*time.Minute)
		if err := pollAllNodes(len(newNodes), 10*time.Minute, func(n int) bool {
			line, _, _, e := d.ConsensusInfo(ctx, n)
			return e == nil && line == "0.15"
		}); err != nil {
			t.Fatalf("the upgraded nodes did not adopt 0.15: %v", err)
		}
	})

	t.Run("switch/04_the_old_binary_is_left_out_of_the_committee", func(t *testing.T) {
		members, err := d.GetElectionMembers(ctx, 1, riseEpoch)
		if err != nil {
			t.Fatalf("election %d members: %v", riseEpoch, err)
		}
		for _, m := range members {
			if m == w(oldNode) {
				t.Fatalf("election %d (the first on 0.15) still seats %s, which runs 0.14: %v", riseEpoch, m, members)
			}
		}
		if len(members) != len(newNodes) {
			t.Fatalf("election %d seats %v, want the four upgraded witnesses", riseEpoch, members)
		}
	})

	var afterUsed int64
	must("after/05_the_upgraded_nodes_charge_1_and_agree", func(t *testing.T) {
		o := rcpMeasure(t, d, ctx, w(2), cid, "setBlob", blob("b1"), 500000, nil)
		if o.Status != "CONFIRMED" {
			t.Fatalf("status %s", o.Status)
		}
		afterUsed = o.Used
		if saved, want := beforeUsed-afterUsed, int64(18*rcpSize); saved != want {
			t.Fatalf("the same call cost %d before and %d after: saved %d, want %d", beforeUsed, afterUsed, saved, want)
		}
		rcpNodesAgreeOnRc(t, d, ctx, newNodes, []string{payer})
	})

	t.Run("after/06_the_old_binary_does_not_hold_the_new_charge", func(t *testing.T) {
		// What the old node did with the same call: it charges 19, or it stopped
		// following the chain. Either way it is no longer a source of truth for
		// RC or dry runs, which is why every node upgrades before the switch.
		want, _, err := rcpLatest(ctx, d, rcpRefNode, payer)
		if err != nil {
			t.Fatalf("magi-%d record: %v", rcpRefNode, err)
		}
		deadline := devnetNow().Add(4 * time.Minute)
		var got rcpRec
		var ok bool
		for devnetNow().Before(deadline) {
			got, ok, _ = rcpLatest(ctx, d, oldNode, payer)
			if ok && got.Height >= want.Height {
				break
			}
			devnetSleep(3 * time.Second)
		}
		switch {
		case !ok || got.Height < want.Height:
			t.Logf("the old node has no record for the call at %d (its newest is at %d): it stopped following", want.Height, got.Height)
		case got.Amount == want.Amount:
			t.Fatalf("the old node holds the same RC record as the upgraded ones (%d at %d): it cannot be charging 19", got.Amount, got.Height)
		default:
			t.Logf("after the switch the old node froze %d RC for the payer, the upgraded nodes %d", got.Amount, want.Amount)
		}
		rc, okSim, msg, err := rcpSim(ctx, d, oldNode, w(2), cid, "setBlob", blob("b2"), 100000)
		t.Logf("a dry run on the old node reports %d RC (ok=%v %s %v); the chain charges %d", rc, okSim, msg, err, afterUsed)
		if err == nil && okSim && rc == afterUsed {
			t.Fatalf("the old node's dry run reports the new price")
		}
	})

	t.Run("after/07_the_chain_keeps_advancing_without_the_old_node", func(t *testing.T) {
		head := nodeBlock(t, d, ctx, 1)
		for _, n := range newNodes {
			if err := d.WaitForBlockProcessing(ctx, n, head+20, 8*time.Minute); err != nil {
				dumpDiagnostics(t, d, ctx)
				t.Fatalf("magi-%d stalled: %v", n, err)
			}
		}
	})

	must("upgrade/08_the_old_node_is_upgraded_and_replays", func(t *testing.T) {
		if err := d.rhUpgradeNode(ctx, oldNode); err != nil {
			t.Fatalf("upgrading magi-%d: %v", oldNode, err)
		}
		if err := pollNode(oldNode, 12*time.Minute, func() bool {
			logs, e := d.Logs(ctx, fmt.Sprintf("magi-%d", oldNode))
			return e == nil && strings.Contains(logs, "consensus-version lag")
		}); err != nil {
			t.Fatalf("magi-%d did not start a version-lag replay after the upgrade: %v", oldNode, err)
		}
		head := nodeBlock(t, d, ctx, 1)
		if err := d.WaitForBlockProcessing(ctx, oldNode, head, 40*time.Minute); err != nil {
			t.Fatalf("magi-%d did not replay to block %d: %v", oldNode, head, err)
		}
	})

	t.Run("upgrade/09_after_the_replay_it_holds_the_same_rc_records", func(t *testing.T) {
		rcpNodesAgreeOnRc(t, d, ctx, []int{rcpRefNode, oldNode}, []string{payer})
	})

	t.Run("upgrade/10_its_dry_run_reports_the_new_price", func(t *testing.T) {
		rc, ok, msg, err := rcpSim(ctx, d, oldNode, w(2), cid, "setBlob", blob("b3"), 100000)
		if err != nil || !ok || rc != afterUsed {
			t.Fatalf("dry run on the upgraded node: %d RC ok=%v %s %v, want %d", rc, ok, msg, err, afterUsed)
		}
	})

	t.Run("upgrade/11_it_is_seated_again", func(t *testing.T) {
		if err := pollNode(1, 30*time.Minute, func() bool {
			last, _, err := rhLatestElection(ctx, d, 1)
			if err != nil {
				return false
			}
			members, err := d.GetElectionMembers(ctx, 1, last)
			if err != nil {
				return false
			}
			for _, m := range members {
				if m == w(oldNode) {
					return true
				}
			}
			return false
		}); err != nil {
			t.Fatalf("%s was not seated again after the upgrade: %v", w(oldNode), err)
		}
	})
}
