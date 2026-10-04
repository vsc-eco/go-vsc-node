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

// TestTssBanOutlivesFaultStallsWithSilentPeer (TSS-BAN-STALL, seen on the shared
// testnet 2026-09-29/30): a node that earned error blames stays banned after the
// fault is gone, and the ban cap only guarantees threshold+1 members after BANS.
// One more member that is elected but never ready then leaves fewer than
// threshold+1, and nothing re-admits the banned (now healthy) node, so reshares
// and signing stop for as long as the ban window (27 elections) lasts. POA-8 does
// not step in because a ban already left a new-committee member out.
//
// Nodes 4 and 5 are built from TSS_BAN_FAULT_SOURCE, a copy of this tree whose
// node (a) empties the VCommitment of the reshare round-1 message it sends to
// other nodes while /tmp/ban-fault exists (tss-lib's ValidateBasic names the real
// author), and (b) skips its readiness attestation while /tmp/no-ready exists.
//
//  1. baseline reshare held by all 5
//  2. magi.test5 faults until a blame naming it lands, then the fault is removed
//  3. control A: everyone ready, magi.test5 healthy, 2 boundaries
//  4. magi.test4 stops attesting readiness (elected, online, producing): 6 boundaries
//  5. control B: magi.test4 ready again -> a reshare must land
//
// Expected RED while the failure state exists: no reshare lands in step 4 although
// four healthy members (1, 2, 3, 5) are online and 4 is the threshold+1 for n=5.
func TestTssBanOutlivesFaultStallsWithSilentPeer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	faultSrc := os.Getenv("TSS_BAN_FAULT_SOURCE")
	if faultSrc == "" {
		t.Skip("TSS_BAN_FAULT_SOURCE not set (a copy of this tree with the /tmp/ban-fault + /tmp/no-ready devnet patch)")
	}
	const faulty, silent = 5, 4
	cfg := tssTestConfig()
	cfg.Nodes = 5
	cfg.GenesisNode = 1
	cfg.OldCodeSourceDir = faultSrc
	cfg.OldCodeNodes = []int{silent, faulty}
	cfg.OldCodeSysconfig = true
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnet(t, cfg, 100*time.Minute)

	faultyAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, faulty)
	silentAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, silent)
	grepLogs := func(node int, pattern string) string {
		gctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		out, _ := exec.CommandContext(gctx, "bash", "-c",
			fmt.Sprintf("docker logs %s 2>&1 | grep -E %q | tail -300", d.containerName(node), pattern)).CombinedOutput()
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
	toggle := func(node int, file string, on bool) {
		arg := "touch"
		if !on {
			arg = "rm -f"
		}
		out, err := exec.CommandContext(ctx, "docker", "exec", d.containerName(node), "sh", "-c", arg+" "+file).CombinedOutput()
		if err != nil {
			t.Fatalf("toggle %s on magi-%d to %v: %v %s", file, node, on, err, out)
		}
	}
	window := func(label string, from, to int) (reshares int, holders [][]string, blames map[string]int, accused map[string]int) {
		blames, accused = map[string]int{}, map[string]int{}
		docs, _ := d.GetCommitments(ctx, 1, bson.M{"block_height": bson.M{"$gt": uint64(from), "$lte": uint64(to)}})
		for _, c := range docs {
			who := named(c)
			t.Logf("%s: %-14s at %d epoch %d -> %v", label, c.Type, c.BlockHeight, c.Epoch, who)
			switch c.Type {
			case "reshare":
				reshares++
				holders = append(holders, who)
			case "blame":
				for _, a := range who {
					blames[a]++
				}
			case "reshare_accuse":
				for _, a := range who {
					accused[a]++
				}
			}
		}
		return
	}
	// Why each party was left out of the reshares in a block range, from magi-1's view.
	exclusions := func(from, to int) map[string]int {
		out := map[string]int{}
		for _, ln := range strings.Split(grepLogs(1, "excluding .* from new committee"), "\n") {
			var bh int
			if i := strings.Index(ln, "sessionId=reshare-"); i >= 0 {
				fmt.Sscanf(ln[i+len("sessionId=reshare-"):], "%d", &bh)
			}
			if bh <= from || bh > to {
				continue
			}
			reason := strings.SplitN(strings.SplitN(ln, "] ", 2)[1], " module=", 2)[0]
			acct := ""
			if i := strings.Index(ln, "account="); i >= 0 {
				acct = strings.Fields(ln[i+len("account="):])[0]
			}
			out[acct+": "+reason]++
		}
		return out
	}

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)

	// 1. Baseline: a reshare every member holds, so magi.test5 is an old party.
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

	// 2. Fault until a blame naming magi.test5 lands, then "upgrade".
	faultStart, _ := getHeadBlock(d.HiveRPCEndpoint())
	toggle(faulty, "/tmp/ban-fault", true)
	t.Logf("2. %s faults from block %d", faultyAcct, faultStart)
	blame, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "blame", "block_height": bson.M{"$gt": uint64(faultStart)}}, 10*time.Minute)
	for err == nil && !contains(named(*blame), faultyAcct) {
		blame, err = d.WaitForCommitment(ctx, 1, bson.M{"type": "blame", "block_height": bson.M{"$gt": blame.BlockHeight}}, 10*time.Minute)
	}
	toggle(faulty, "/tmp/ban-fault", false)
	fixed, _ := getHeadBlock(d.HiveRPCEndpoint())
	faultLinesAtFix := strings.Count(grepLogs(faulty, "BAN-FAULT"), "BAN-FAULT")
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: no blame naming %s landed while it faulted: %v (fault log: %q)", faultyAcct, err, lastLine(grepLogs(faulty, "BAN-FAULT")))
	}
	t.Logf("2. blame naming %s landed at %d; fault removed at block %d", faultyAcct, blame.BlockHeight, fixed)
	window("2. fault window", faultStart, fixed)

	// 3. Control A: everyone ready, magi.test5 healthy.
	ctrlAEnd := nextReshareBoundary(fixed) + 2*testRotateInterval
	waitForBlock(t, d.HiveRPCEndpoint(), ctrlAEnd+45, 8*time.Minute)
	resA, holdersA, _, _ := window("3. control A", fixed, ctrlAEnd+45)
	t.Logf("3. control A (all ready, %s healthy): reshares=%d holders=%v exclusions=%v", faultyAcct, resA, holdersA, exclusions(fixed, ctrlAEnd))

	// 4. magi.test4 elected + online but never ready.
	silentStart, _ := getHeadBlock(d.HiveRPCEndpoint())
	toggle(silent, "/tmp/no-ready", true)
	const boundaries = 6
	silentEnd := nextReshareBoundary(silentStart) + boundaries*testRotateInterval
	t.Logf("4. %s stops attesting readiness at %d; observing until %d", silentAcct, silentStart, silentEnd)
	waitForBlock(t, d.HiveRPCEndpoint(), silentEnd+45, 15*time.Minute)
	res4, holders4, blames4, accused4 := window("4. silent window", silentStart, silentEnd+45)
	excl4 := exclusions(silentStart, silentEnd)
	epochNow, _ := latestElection(t, d, ctx, 1)
	members, _ := d.GetElectionMembers(ctx, 1, epochNow)
	stillBanned := grepLogs(1, "node banned.*account="+faultyAcct)
	t.Logf("4. silent window: reshares=%d holders=%v blames=%v accusations=%v", res4, holders4, blames4, accused4)
	t.Logf("4. exclusions seen by magi-1: %v", excl4)
	t.Logf("4. committee at epoch %d: %v; %s ban (magi-1, last): %s", epochNow, members, faultyAcct, lastLine(stillBanned))
	if n := strings.Count(grepLogs(faulty, "BAN-FAULT"), "BAN-FAULT"); n != faultLinesAtFix {
		t.Fatalf("PRECONDITION FAILED: %s kept faulting after the fix (%d fault lines at fix, %d now)", faultyAcct, faultLinesAtFix, n)
	}
	t.Logf("4. %s sent no faulty message since the fix (%d fault lines, all before block %d)", faultyAcct, faultLinesAtFix, fixed)
	// What a 3-party session does from inside the party (magi-1 is old+new party 0):
	// the first reshare session that actually ran in the window, and how many
	// reshares the parties completed that never got a commitment on chain.
	firstSilent := 0
	for _, ln := range strings.Split(grepLogs(1, "starting reshare"), "\n") {
		var bh int
		if i := strings.Index(ln, "sessionId=reshare-"); i >= 0 {
			fmt.Sscanf(ln[i+len("sessionId=reshare-"):], "%d", &bh)
		}
		if bh > silentStart && bh <= silentEnd && (firstSilent == 0 || bh < firstSilent) {
			firstSilent = bh
		}
	}
	for n := 1; n <= 3; n++ {
		ok := 0
		for _, ln := range strings.Split(grepLogs(n, "reshare success"), "\n") {
			var bh int
			if i := strings.Index(ln, "blockHeight="); i >= 0 {
				fmt.Sscanf(ln[i+len("blockHeight="):], "%d", &bh)
			}
			if bh > silentStart && bh <= silentEnd {
				ok++
			}
		}
		t.Logf("4. magi-%d completed %d reshare(s) in the silent window at the btss level (on chain: %d)", n, ok, res4)
	}
	var sess []string
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Minute)
	sessOut, _ := exec.CommandContext(sctx, "bash", "-c", fmt.Sprintf(
		"docker logs %s 2>&1 | grep -F 'sessionId=reshare-%d-' | grep -v -E 'type=ready|reconnect failed|received reshare message|SendMsg success|message processed successfully' | head -80",
		d.containerName(1), firstSilent)).CombinedOutput()
	scancel()
	for _, ln := range strings.Split(strings.TrimSpace(string(sessOut)), "\n") {
		if ln == "" || strings.Contains(ln, "type=ready") || strings.Contains(ln, "reconnect failed") ||
			strings.Contains(ln, "received reshare message") || strings.Contains(ln, "SendMsg success") ||
			strings.Contains(ln, "message processed successfully") {
			continue
		}
		if len(ln) > 400 {
			ln = ln[:400]
		}
		sess = append(sess, ln)
	}
	t.Logf("4. magi-1 on the silent-window session at %d:\n%s", firstSilent, strings.Join(sess, "\n"))
	t.Logf("4. insufficient/pre-flight lines on magi-1: %s", lastLine(grepLogs(1, "insufficient|pre-flight check failed|not enough")))

	// 5. Control B: magi.test4 ready again.
	toggle(silent, "/tmp/no-ready", false)
	ctrlB, errB := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(silentEnd)}}, 8*time.Minute)
	if errB != nil {
		t.Errorf("CONTROL B FAILED: no reshare within 8 min after %s attested again: %v", silentAcct, errB)
	} else {
		t.Logf("5. control B: reshare at %d held by %v", ctrlB.BlockHeight, named(*ctrlB))
	}

	shrunk := false
	for _, h := range holders4 {
		if len(h) < 4 {
			shrunk = true
		}
	}
	switch {
	case res4 == 0:
		t.Errorf("TSS-BAN-STALL: no reshare in %d boundaries with %s healthy (fault removed at %d) and only %s not ready: the ban outlives the fault and the ban cap does not count non-ready members", boundaries, faultyAcct, fixed, silentAcct)
	case shrunk:
		t.Errorf("TSS-BAN-STALL (shrink variant): reshares landed but held by fewer than threshold+1=4 of 5: %v", holders4)
	default:
		t.Logf("no stall: %d reshare(s) landed held by %v", res4, holders4)
	}
}
