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

// TestTssKeyShrinkStopsAtCommitteeMajority (item 4 fix, 0.9.0): with 9 members
// and 3 honest ones not ready, the 6 left reach the BLS commit quorum, so before
// the fix a reshare landed among 6 at threshold 3: 4 of 9 could sign. At 0.9.0
// a reshare must keep signing above half the committee (threshold >= 4, at least
// 7 parties), so it waits; the old key keeps working and the next reshare after
// they return is held by all 9.
//
// Expected GREEN with the fix: no reshare lands while the 3 are quiet, and the
// control reshare afterwards is held by all 9.
func TestTssKeyShrinkStopsAtCommitteeMajority(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	src := os.Getenv("TSS_BAN_FAULT_SOURCE")
	if src == "" {
		t.Skip("TSS_BAN_FAULT_SOURCE not set (a copy of this tree with the /tmp/no-ready devnet patch)")
	}
	quiet := []int{7, 8, 9}
	cfg := tssTestConfig()
	cfg.Nodes = 9
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
	t.Logf("2. magi-7, magi-8 and magi-9 stop attesting readiness at %d", start)
	shrunk, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(start + 5)}}, 12*time.Minute)
	stop, _ := getHeadBlock(d.HiveRPCEndpoint())
	refusals := grepLogs(1, "reshare would lower the key threshold")
	for _, n := range quiet {
		toggle(n, false)
	}
	if err == nil {
		holders := named(*shrunk)
		t.Logf("2. reshare at %d (epoch %d) held by %d: %v", shrunk.BlockHeight, shrunk.Epoch, len(holders), holders)
		if len(holders) < 7 {
			t.Errorf("KEY-SHRINK: with 3 of 9 not ready, a reshare landed held by %d (%v): fewer than a committee majority can sign it", len(holders), holders)
		}
	} else {
		t.Logf("2. no reshare landed between %d and %d while 3 of 9 were quiet", start, stop)
	}
	if refusals == "" {
		t.Errorf("magi-1 never logged a threshold-floor refusal while 3 of 9 were quiet")
	} else {
		lines := strings.Split(refusals, "\n")
		t.Logf("2. magi-1 refused %d time(s), last: %s", len(lines), lines[len(lines)-1])
	}

	back, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": stop}}, 12*time.Minute)
	if err != nil {
		t.Fatalf("3. control: no reshare within 12 min after the 3 returned: %v", err)
	}
	if h := named(*back); len(h) != cfg.Nodes {
		t.Errorf("3. control: reshare at %d held by %d (%v), want all %d", back.BlockHeight, len(h), h, cfg.Nodes)
	} else {
		t.Logf("3. control: reshare at %d held by all %d", back.BlockHeight, cfg.Nodes)
	}
}
