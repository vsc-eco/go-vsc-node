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

// TestTssFrame1SsidLiarFramesNobody (TSS-FRAME-1): magi.test1 is old party 0 of
// every reshare (party keys sort by account bytes), so its round-1 SSID is the
// reference every new party compares the others against. Built from
// TSS_FRAME1_LIAR_SOURCE, a copy of this tree whose node forges that SSID on the
// messages it sends to other nodes while /tmp/frame1-lie exists in its container
// (its own new party still sees the genuine one, as a real liar's would).
//
// Before 0.9.0 each honest new party names old party 1 (magi.test2) and the five
// of seven that agree carry the blame over the 2/3 bar, so an honest node is
// blamed, left out by the 33% rule and eventually banned. At 0.9.0 nobody is
// named. Precondition: honest nodes really saw the forged SSID (else the run
// proves nothing). Control: once the liar stops, a reshare lands.
//
// Run A (expected RED): this test and a liar tree on the parent commit.
// Run B (expected GREEN): both on the fix commit.
func TestTssFrame1SsidLiarFramesNobody(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	liarSrc := os.Getenv("TSS_FRAME1_LIAR_SOURCE")
	if liarSrc == "" {
		t.Skip("TSS_FRAME1_LIAR_SOURCE not set (a copy of this tree whose node forges its reshare SSID while /tmp/frame1-lie exists)")
	}
	const liar = 1
	cfg := tssTestConfig()
	cfg.Nodes = 7 // the framed blame needs n-2 >= 2n/3 signers, so 6 members or more
	cfg.GenesisNode = 7
	cfg.OldCodeSourceDir = liarSrc
	cfg.OldCodeNodes = []int{liar}
	cfg.OldCodeSysconfig = true
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnet(t, cfg, 80*time.Minute)

	liarAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, liar)
	framedAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, 2)
	grepLogs := func(node int, pattern string) string {
		gctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, _ := exec.CommandContext(gctx, "bash", "-c",
			fmt.Sprintf("docker logs %s 2>&1 | grep -E %q | tail -200", d.containerName(node), pattern)).CombinedOutput()
		return strings.TrimSpace(string(out))
	}
	named := func(c TssCommitmentDoc) []string {
		mem, _ := d.GetElectionMembers(ctx, 2, c.Epoch)
		bits := decodeBitset(t, c.Commitment)
		var out []string
		for j, m := range mem {
			if bits.Bit(j) == 1 {
				out = append(out, strings.TrimPrefix(m, "hive:"))
			}
		}
		return out
	}

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	// Baseline: a reshare every member holds, so the liar is old party 0 and
	// magi.test2 is old party 1 of the next sessions.
	var base *TssCommitmentDoc
	for tries := 0; base == nil; tries++ {
		if tries == 8 {
			t.Fatalf("PRECONDITION FAILED: no reshare held by all %d members", cfg.Nodes)
		}
		head, _ := getHeadBlock(d.HiveRPCEndpoint())
		r, err := d.WaitForCommitment(ctx, 2, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(head)}}, 8*time.Minute)
		if err != nil {
			t.Fatalf("PRECONDITION FAILED: no baseline reshare: %v", err)
		}
		if h := named(*r); len(h) == cfg.Nodes {
			base = r
		} else {
			t.Logf("reshare at %d held by %v, waiting for one with all %d", r.BlockHeight, h, cfg.Nodes)
		}
	}
	t.Logf("baseline reshare held by all %d at %d (epoch %d)", cfg.Nodes, base.BlockHeight, base.Epoch)

	lie := func(on bool) {
		arg := "touch"
		if !on {
			arg = "rm -f"
		}
		out, err := exec.CommandContext(ctx, "docker", "exec", d.containerName(liar), "sh", "-c", arg+" /tmp/frame1-lie").CombinedOutput()
		if err != nil {
			t.Fatalf("switching the lie %v: %v %s", on, err, out)
		}
	}
	start, _ := getHeadBlock(d.HiveRPCEndpoint())
	lie(true)
	t.Logf("%s lies from block %d", liarAcct, start)
	const sessions = 10
	end := nextReshareBoundary(start) + sessions*testRotateInterval
	waitForBlock(t, d.HiveRPCEndpoint(), end, 20*time.Minute)
	lie(false)
	// Let the last session time out and its commitments land.
	waitForBlock(t, d.HiveRPCEndpoint(), end+45, 5*time.Minute)

	docs, _ := d.GetCommitments(ctx, 2, bson.M{"block_height": bson.M{"$gt": uint64(start), "$lte": uint64(end + 45)}})
	reshares, blames, blamesNamingHonest, blamesNamingFramed := 0, 0, 0, 0
	accused := map[string]int{}
	for _, c := range docs {
		who := named(c)
		t.Logf("in window: %-14s at %d epoch %d -> %v", c.Type, c.BlockHeight, c.Epoch, who)
		switch c.Type {
		case "reshare":
			reshares++
		case "blame":
			blames++
			honest := false
			for _, a := range who {
				if a != liarAcct {
					honest = true
				}
				if a == framedAcct {
					blamesNamingFramed++
				}
			}
			if honest {
				blamesNamingHonest++
			}
		case "reshare_accuse":
			for _, a := range who {
				accused[a]++
			}
		}
	}

	sawForged, sawDropped := 0, 0
	var computed []string
	for n := 2; n <= cfg.Nodes; n++ {
		if lines := grepLogs(n, "ssid mismatch"); lines != "" {
			sawForged++
			computed = append(computed, fmt.Sprintf("magi-%d: %s", n, lastLine(lines)))
		}
		if grepLogs(n, "did not author the bad data") != "" {
			sawDropped++
		}
	}
	bans := grepLogs(2, "node banned")
	t.Logf("liar log: %s", lastLine(grepLogs(liar, "FRAME1 LIAR")))
	for _, c := range computed {
		t.Logf("honest verdict %s", c)
	}
	t.Logf("window %d..%d (%d sessions, floor 0.%d): reshares=%d blames=%d naming honest=%d naming %s=%d; honest nodes that saw the forged SSID=%d, that dropped the culprit=%d; accusations=%v",
		start, end, sessions, poaDevnetFloor(), reshares, blames, blamesNamingHonest, framedAcct, blamesNamingFramed, sawForged, sawDropped, accused)
	if bans != "" {
		t.Logf("bans seen by magi-2:\n%s", bans)
	}

	// Control: with the liar honest again the key rotates.
	if rec, err := d.WaitForCommitment(ctx, 2, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(end)}}, 8*time.Minute); err != nil {
		t.Errorf("CONTROL FAILED: no reshare within 8 min after the liar stopped: %v", err)
	} else {
		t.Logf("control: reshare at %d held by %v", rec.BlockHeight, named(*rec))
		// Why anyone was left out of it: blame, ban, accusation or readiness.
		session := fmt.Sprintf("sessionId=reshare-%d-", rec.BlockHeight)
		for _, n := range []int{2, 3} {
			var why []string
			for _, ln := range strings.Split(grepLogs(n, session), "\n") {
				if strings.Contains(ln, "exclud") || strings.Contains(ln, "accus") || strings.Contains(ln, "participant selection") {
					why = append(why, ln)
				}
			}
			t.Logf("magi-%d on the control reshare:\n%s", n, strings.Join(why, "\n"))
		}
	}

	if sawForged == 0 {
		t.Fatalf("PRECONDITION FAILED: no honest node logged an ssid mismatch, so the liar never lied and the run proves nothing")
	}
	if blamesNamingHonest > 0 {
		t.Errorf("a lying old party 0 got %d blame(s) naming an honest node on chain (%d naming %s)", blamesNamingHonest, blamesNamingFramed, framedAcct)
	}
	if strings.Contains(bans, framedAcct) {
		t.Errorf("the framed node %s was banned", framedAcct)
	}
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return lines[len(lines)-1]
}
