package devnet

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestTssKeyShrinksWhenHonestNodesAreNotReady (item 4, key shrink): a reshare takes
// only the members that are ready at that block, and the new key's threshold follows
// that set, not the committee. The only floor is the BLS commit quorum (2/3 of the
// committee), so with 7 members a reshare among 5 lands and leaves a key that 4
// parties can sign (GetThreshold(5)+1) instead of 5 (GetThreshold(7)+1). Colluders
// cannot get there by going quiet themselves; they need HONEST members out (gossip
// DoS, POA-8, bans). Here two honest members simply do not attest readiness, standing
// in for a DoS on them.
//
// magi.test6 and magi.test7 are built from TSS_BAN_FAULT_SOURCE (the /tmp/no-ready
// toggle). Expected RED while the gap exists: a reshare commitment lands held by 5
// of 7, and its parties computed newThreshold=3.
func TestTssKeyShrinksWhenHonestNodesAreNotReady(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	src := os.Getenv("TSS_BAN_FAULT_SOURCE")
	if src == "" {
		t.Skip("TSS_BAN_FAULT_SOURCE not set (a copy of this tree with the /tmp/no-ready devnet patch)")
	}
	quiet := []int{6, 7}
	cfg := tssTestConfig()
	cfg.Nodes = 7
	cfg.GenesisNode = 1
	cfg.OldCodeSourceDir = src
	cfg.OldCodeNodes = quiet
	cfg.OldCodeSysconfig = true
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnet(t, cfg, 80*time.Minute)

	grepLogs := func(node int, pattern string) string {
		gctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, _ := exec.CommandContext(gctx, "bash", "-c",
			fmt.Sprintf("docker logs %s 2>&1 | grep -E %q | tail -200", d.containerName(node), pattern)).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	named := func(c TssCommitmentDoc) []string {
		mem, _ := d.GetElectionMembers(ctx, 1, c.Epoch)
		bits := decodeBitset(t, c.Commitment)
		var out []string
		for j, m := range mem {
			if bits.Bit(j) == 1 {
				out = append(out, strings.TrimPrefix(m, "hive:"))
			}
		}
		return out
	}
	toggle := func(node int, on bool) {
		arg := "touch"
		if !on {
			arg = "rm -f"
		}
		out, err := exec.CommandContext(ctx, "docker", "exec", d.containerName(node), "sh", "-c", arg+" /tmp/no-ready").CombinedOutput()
		if err != nil {
			t.Fatalf("toggle no-ready on magi-%d: %v %s", node, err, out)
		}
	}

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	var base *TssCommitmentDoc
	for tries := 0; base == nil; tries++ {
		if tries == 8 {
			t.Fatalf("PRECONDITION FAILED: no reshare held by all %d members", cfg.Nodes)
		}
		head, _ := getHeadBlock(d.HiveRPCEndpoint())
		r, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(head)}}, 8*time.Minute)
		if err != nil {
			t.Fatalf("PRECONDITION FAILED: no baseline reshare: %v", err)
		}
		if h := named(*r); len(h) == cfg.Nodes {
			base = r
		} else {
			t.Logf("reshare at %d held by %v, waiting for one with all %d", r.BlockHeight, h, cfg.Nodes)
		}
	}
	t.Logf("1. baseline reshare held by all %d at %d (epoch %d)", cfg.Nodes, base.BlockHeight, base.Epoch)

	start, _ := getHeadBlock(d.HiveRPCEndpoint())
	for _, n := range quiet {
		toggle(n, true)
	}
	t.Logf("2. magi-6 and magi-7 stop attesting readiness at %d", start)
	shrunk, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(start + 5)}}, 10*time.Minute)
	for _, n := range quiet {
		toggle(n, false)
	}
	if err != nil {
		t.Fatalf("no reshare landed while 2 of 7 were not ready: %v", err)
	}
	holders := named(*shrunk)
	session := fmt.Sprintf("sessionId=reshare-%d-", shrunk.BlockHeight)
	var thr string
	for _, ln := range strings.Split(grepLogs(1, "reshare thresholds calculated"), "\n") {
		if strings.Contains(ln, session) {
			thr = ln
		}
	}
	t.Logf("2. reshare at %d (epoch %d) held by %d: %v", shrunk.BlockHeight, shrunk.Epoch, len(holders), holders)
	t.Logf("2. magi-1 thresholds for it: %s", thr)

	// Control: once everyone is ready again, the next reshare is back to all 7.
	back, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": shrunk.BlockHeight}}, 8*time.Minute)
	if err == nil {
		t.Logf("3. control: next reshare at %d held by %v", back.BlockHeight, named(*back))
	} else {
		t.Logf("3. control: no reshare within 8 min: %v", err)
	}

	if len(holders) < cfg.Nodes && strings.Contains(thr, "newThreshold=3") && strings.Contains(thr, "newTotal=5") {
		t.Errorf("KEY-SHRINK: with 2 honest members of 7 not ready, a reshare landed held by %d (%v) at threshold 3: 4 parties can sign this key epoch instead of 5", len(holders), holders)
	} else {
		t.Logf("no shrink observed: holders=%v thresholds=%q", holders, thr)
	}
}
