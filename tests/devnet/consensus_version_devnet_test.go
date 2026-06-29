package devnet

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/common/params"
	systemconfig "vsc-node/modules/common/system-config"
)

// TestConsensusVersionDevnetManual drives the multi-candidate vsc.propose_consensus_version
// rollout end-to-end on a real docker devnet:
//
//  1. Pin the consensus-version floor ONE BELOW the running binary (floor 0.2, nodes
//     run 0.3) so there is a valid higher target to propose to.
//  2. A committee witness broadcasts vsc.propose_consensus_version targeting 0.3.
//  3. localNodeInfo.consensus_version_proposals shows the pending candidate.
//  4. Because every node announces 0.3 (≥80% stake-ready), the next election adopts
//     the floor → active_consensus_line rises to 0.3 on every node.
//  5. The election_result GC prunes the now-adopted candidate.
//
// Recovery checklist (suspend / require_version) is still operator-driven — see the
// notes below.
//
//	CONSENSUS_DEVNET_E2E=1 go test -v -run TestConsensusVersionDevnetManual -timeout 30m ./tests/devnet/
func TestConsensusVersionDevnetManual(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet consensus test in short mode")
	}
	if os.Getenv("CONSENSUS_DEVNET_E2E") == "" {
		t.Skip("set CONSENSUS_DEVNET_E2E=1 to run devnet-backed consensus checks (see doc comment)")
	}

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{
		"DEVNET_DETERMINISTIC_BLS": "1", // deterministic witness keys so genesis-elector forms a valid committee
	}
	// Floor pinned to 0.2 (one below the running 0.3 binary) so 0.3 is a valid
	// propose target; short election interval to iterate epochs fast.
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval:               20,
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 2,
		},
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, ctx := startDevnetNoKey(t, cfg, 30*time.Minute)

	const proposer = "magi.test1"

	// 1) Wait until the floor has settled at 0.2 and the nodes report running 0.3.
	deadline := time.Now().Add(8 * time.Minute)
	var line, running string
	for time.Now().Before(deadline) {
		var err error
		line, running, _, err = d.ConsensusInfo(ctx, 1)
		if err == nil && line == "0.2" && strings.HasPrefix(running, "0.3") {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if line != "0.2" || !strings.HasPrefix(running, "0.3") {
		t.Fatalf("[cv] expected active line 0.2 with running 0.3.x, got line=%q running=%q", line, running)
	}
	_, epoch, err := d.LocalNodeInfo(ctx, 1)
	if err != nil {
		t.Fatalf("[cv] localNodeInfo: %v", err)
	}
	t.Logf("[cv] floor stable at 0.2, nodes run %s, epoch=%d", running, epoch)

	// 2) Propose 0.3 (activation next epoch) from a committee witness. net_id omitted
	// (empty matches any network — the handler only checks it when set).
	payload := map[string]interface{}{
		"major": 0, "consensus": 3, "activation_epoch": epoch + 1,
	}
	tx, err := d.BroadcastCustomJSON("vsc.propose_consensus_version", []string{proposer}, payload, d.cfg.InitminerWIF)
	if err != nil {
		t.Fatalf("[cv] broadcasting propose: %v", err)
	}
	t.Logf("[cv] proposed 0.3 from %s tx=%s", proposer, tx)

	// 3) The candidate must be recorded on every node. Proposal targets render via
	// Version.Format() as the 3-component "0.3.0" (non_consensus=0), unlike
	// active_consensus_line below which is the 2-component coordinated line "0.3".
	//
	// Adoption is fast: every node announces 0.3, so the proposal clears the ≥80%
	// readiness guard and is adopted + GC'd within an election. The transient "pending
	// candidate" state crosses each node at a slightly different time, so requiring all
	// five to show it in one snapshot is racy. Latch per node instead: a node counts as
	// having recorded the proposal once it EITHER shows the pending candidate OR has
	// already advanced its floor to 0.3 (only reachable by processing the proposal). A
	// node genuinely stuck at 0.2 that never surfaces the candidate still fails.
	recorded := make(map[int]bool, cfg.Nodes)
	if err := pollAllNodes(cfg.Nodes, 3*time.Minute, func(node int) bool {
		if recorded[node] {
			return true
		}
		line, _, targets, e := d.ConsensusInfo(ctx, node)
		if e == nil && (containsStr(targets, "0.3.0") || line == "0.3") {
			recorded[node] = true
		}
		return recorded[node]
	}); err != nil {
		t.Fatalf("[cv] candidate proposal never recorded on all nodes: %v", err)
	}
	t.Log("[cv] candidate 0.3 recorded on all nodes")

	// 4) Floor must rise to 0.3 on every node once the readiness guard adopts it.
	if err := pollAllNodes(cfg.Nodes, 8*time.Minute, func(node int) bool {
		l, _, _, e := d.ConsensusInfo(ctx, node)
		return e == nil && l == "0.3"
	}); err != nil {
		t.Fatalf("[cv] floor did not adopt 0.3 across all nodes: %v", err)
	}
	t.Log("[cv] floor adopted 0.3 on all nodes")

	// 5) The adopted candidate must be garbage-collected (pruned) by the election_result handler.
	if err := pollAllNodes(cfg.Nodes, 3*time.Minute, func(node int) bool {
		_, _, targets, e := d.ConsensusInfo(ctx, node)
		return e == nil && !containsStr(targets, "0.3.0")
	}); err != nil {
		t.Fatalf("[cv] adopted candidate not pruned: %v", err)
	}
	t.Log("[cv] SUCCESS: propose → candidate → adoption → GC, consistent across all nodes")
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// pollAllNodes returns nil once pred holds for every node within timeout.
func pollAllNodes(nodes int, timeout time.Duration, pred func(node int) bool) error {
	deadline := time.Now().Add(timeout)
	for {
		all := true
		var pending int
		for n := 1; n <= nodes; n++ {
			if !pred(n) {
				all = false
				pending = n
				break
			}
		}
		if all {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node-%d did not reach the expected state within %v", pending, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// pollNode returns nil once pred holds within timeout (single-node variant).
func pollNode(node int, timeout time.Duration, pred func() bool) error {
	deadline := time.Now().Add(timeout)
	for {
		if pred() {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("node-%d did not reach the expected state within %v", node, timeout)
		}
		time.Sleep(3 * time.Second)
	}
}

// TestConsensusVersionLateAdoptionReindexDevnet drives a node through a version
// rollout while it is OFFLINE, then exercises the two restart-time code paths the
// versioning system added:
//
//	Phase 1 — restart + late-adoption catch-up. Node-5 is down for the whole rollout,
//	  the rest of the fleet adopts 0.3, and a full epoch later node-5 restarts. It ran
//	  the 0.3 binary throughout (it was merely offline, never diverged), so it must
//	  catch up across the adoption boundary WITHOUT a version-lag reindex. This boots
//	  node-5 a SECOND time with a recorded processed_under_* version, which is the exact
//	  path that used to nil-deref in DbReindex.Init (ChainActiveAt ran before the
//	  elections collection was bound) and panic the node on every restart.
//
//	Phase 2 — forced laggard reindex. In a homogeneous-binary devnet the trigger never
//	  fires naturally (every node records its own running version), so we stop node-5
//	  and seed processed_under_consensus=2 — simulating a node that processed up to its
//	  (now 0.3) head under 0.2 and diverged. On restart the reindex gate compares
//	  processed 0.2 vs chain-active 0.3 at the head and full-reindexes: node-5 wipes
//	  derived state, replays from genesis under the correct rules, and re-adopts 0.3.
//
//	CONSENSUS_DEVNET_E2E=1 go test -v -run TestConsensusVersionLateAdoptionReindexDevnet -timeout 45m ./tests/devnet/
func TestConsensusVersionLateAdoptionReindexDevnet(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet consensus test in short mode")
	}
	if os.Getenv("CONSENSUS_DEVNET_E2E") == "" {
		t.Skip("set CONSENSUS_DEVNET_E2E=1 to run devnet-backed consensus checks (see doc comment)")
	}

	cfg := DefaultConfig()
	cfg.Nodes = 5
	cfg.SkipFunding = true
	cfg.LogLevel = "info"
	cfg.MagiEnv = map[string]string{
		"DEVNET_DETERMINISTIC_BLS": "1",
	}
	// Floor pinned to 0.2 (one below the running 0.3 binary) so 0.3 is a valid
	// propose target; short election interval to iterate epochs fast.
	cfg.SysConfigOverrides = &systemconfig.SysConfigOverrides{
		ConsensusParams: &params.ConsensusParams{
			ElectionInterval:               20,
			ConsensusVersionFloorEpoch:     1,
			ConsensusVersionFloorMajor:     0,
			ConsensusVersionFloorConsensus: 2,
		},
	}
	if os.Getenv("DEVNET_KEEP") != "" {
		cfg.KeepRunning = true
	}

	d, ctx := startDevnetNoKey(t, cfg, 45*time.Minute)

	const proposer = "magi.test1"
	const downNode = 5 // magi.test5 — its announced 0.3 version stays counted while offline

	// 1) Floor settled at 0.2 with nodes running 0.3.
	deadline := time.Now().Add(8 * time.Minute)
	var line, running string
	for time.Now().Before(deadline) {
		var err error
		line, running, _, err = d.ConsensusInfo(ctx, 1)
		if err == nil && line == "0.2" && strings.HasPrefix(running, "0.3") {
			break
		}
		time.Sleep(3 * time.Second)
	}
	if line != "0.2" || !strings.HasPrefix(running, "0.3") {
		t.Fatalf("[cv] expected active line 0.2 with running 0.3.x, got line=%q running=%q", line, running)
	}
	t.Logf("[cv] floor stable at 0.2, nodes run %s", running)

	// 2) Take node-5 down for the whole rollout (the late adopter).
	if err := d.StopNode(ctx, downNode); err != nil {
		t.Fatalf("[cv] stopping node-%d: %v", downNode, err)
	}
	t.Logf("[cv] node-%d stopped before rollout", downNode)

	// 3) Propose 0.3 from a committee witness. activation_epoch 0 → the handler defaults
	// it to the next epoch at processing time, dodging a read-vs-land epoch race.
	payload := map[string]interface{}{"major": 0, "consensus": 3, "activation_epoch": 0}
	if _, err := d.BroadcastCustomJSON("vsc.propose_consensus_version", []string{proposer}, payload, d.cfg.InitminerWIF); err != nil {
		t.Fatalf("[cv] broadcasting propose: %v", err)
	}
	t.Logf("[cv] proposed 0.3 from %s (node-%d offline)", proposer, downNode)

	// 4) The remaining fleet (nodes 1-4) adopts 0.3.
	if err := pollAllNodes(4, 8*time.Minute, func(node int) bool {
		l, _, _, e := d.ConsensusInfo(ctx, node)
		return e == nil && l == "0.3"
	}); err != nil {
		t.Fatalf("[cv] online fleet did not adopt 0.3: %v", err)
	}
	_, adoptEpoch, err := d.LocalNodeInfo(ctx, 1)
	if err != nil {
		t.Fatalf("[cv] localNodeInfo: %v", err)
	}
	t.Logf("[cv] online fleet adopted 0.3 at epoch %d", adoptEpoch)

	// 5) Wait a full epoch past adoption so node-5 comes back clearly behind.
	if err := pollNode(1, 6*time.Minute, func() bool {
		_, ep, e := d.LocalNodeInfo(ctx, 1)
		return e == nil && ep >= adoptEpoch+1
	}); err != nil {
		t.Fatalf("[cv] fleet did not advance a full epoch past adoption: %v", err)
	}

	// ── Phase 1: restart node-5 and catch up across the adoption boundary ──
	if err := d.StartNode(ctx, downNode); err != nil {
		t.Fatalf("[cv] restarting node-%d: %v", downNode, err)
	}
	t.Logf("[cv] node-%d restarted (late adopter)", downNode)

	if err := pollNode(downNode, 10*time.Minute, func() bool {
		l, _, _, e := d.ConsensusInfo(ctx, downNode)
		return e == nil && l == "0.3"
	}); err != nil {
		t.Fatalf("[cv] node-%d did not catch up to floor 0.3 after restart: %v", downNode, err)
	}
	// It never diverged (ran 0.3 throughout), so the catch-up must NOT be a reindex.
	if logs, lerr := d.Logs(ctx, fmt.Sprintf("magi-%d", downNode)); lerr == nil &&
		strings.Contains(logs, "consensus-version lag") {
		t.Fatalf("[cv] node-%d unexpectedly version-lag-reindexed on a plain offline catch-up", downNode)
	}
	t.Logf("[cv] PHASE 1 ok: node-%d restarted and caught up to 0.3 without a reindex", downNode)

	// ── Phase 2: simulate a 0.2 laggard and force the version-lag reindex ──
	if err := d.StopNode(ctx, downNode); err != nil {
		t.Fatalf("[cv] stopping node-%d for seed: %v", downNode, err)
	}
	preBlock, err := d.getLastProcessedBlock(ctx, downNode)
	if err != nil {
		t.Fatalf("[cv] reading node-%d last_processed_block: %v", downNode, err)
	}
	if err := d.seedProcessedUnderVersion(ctx, downNode, 0, 2); err != nil {
		t.Fatalf("[cv] seeding processed_under version: %v", err)
	}
	t.Logf("[cv] node-%d head at block %d tagged processed-under 0.2 (chain-active is 0.3)", downNode, preBlock)

	if err := d.StartNode(ctx, downNode); err != nil {
		t.Fatalf("[cv] restarting node-%d after seed: %v", downNode, err)
	}

	// 8) The version-lag full reindex must fire.
	if err := pollNode(downNode, 5*time.Minute, func() bool {
		logs, e := d.Logs(ctx, fmt.Sprintf("magi-%d", downNode))
		return e == nil && strings.Contains(logs, "consensus-version lag")
	}); err != nil {
		t.Fatalf("[cv] node-%d did not log a consensus-version-lag reindex: %v", downNode, err)
	}
	t.Logf("[cv] node-%d triggered a version-lag full reindex", downNode)

	// 9) After replaying from genesis under the correct rules it must re-adopt 0.3 and
	// climb back past its pre-reindex head — consistent with the fleet.
	if err := pollNode(downNode, 12*time.Minute, func() bool {
		l, _, _, e := d.ConsensusInfo(ctx, downNode)
		if e != nil || l != "0.3" {
			return false
		}
		bh, be := d.getLastProcessedBlock(ctx, downNode)
		return be == nil && bh >= preBlock
	}); err != nil {
		t.Fatalf("[cv] node-%d did not recover (re-adopt 0.3 and catch back up) after reindex: %v", downNode, err)
	}
	t.Logf("[cv] SUCCESS: node-%d survived restart, caught up late, reindexed on simulated lag, and recovered", downNode)
}
