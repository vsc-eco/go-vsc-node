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

	// 3) The candidate must show up in localNodeInfo on every node.
	if err := pollAllNodes(cfg.Nodes, 3*time.Minute, func(node int) bool {
		_, _, targets, e := d.ConsensusInfo(ctx, node)
		return e == nil && containsStr(targets, "0.3")
	}); err != nil {
		t.Fatalf("[cv] candidate proposal not visible on all nodes: %v", err)
	}
	t.Log("[cv] candidate 0.3 visible on all nodes")

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
		return e == nil && !containsStr(targets, "0.3")
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
