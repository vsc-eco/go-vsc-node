package devnet

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// TestTssEquivocationFramesHonestSigner (item 7, a frame path TSS-FRAME-1 does not
// cover): an old-committee member sends ONE new party a different VSS polynomial
// with the same constant term during a reshare. Every per-recipient check passes and
// the node only compares the public key, so the reshare lands. The target's view of
// the other parties' public shares now differs from everyone else's, so in the next
// signing session every honest party names the TARGET ("failed to calculate Bob_mid",
// a correctly attributed tss-lib error that FRAME-1 must not drop). The liar is never
// named.
//
// magi.test1 is built from TSS_EQUIV_SOURCE: this tree with a vendored tss-lib whose
// resharing round 1/3 sends the alternate polynomial to the party named in
// /tmp/frame-equivocate. The lie is switched on for one reshare, then off.
//
// Expected RED while the gap exists: a blame naming the honest target lands and the
// target is excluded from signing, and no commitment names the liar.
func TestTssEquivocationFramesHonestSigner(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	src := os.Getenv("TSS_EQUIV_SOURCE")
	if src == "" {
		t.Skip("TSS_EQUIV_SOURCE not set (a copy of this tree with the equivocating tss-lib)")
	}
	const liar, target = 1, 2
	cfg := tssTestConfig()
	cfg.Nodes = 7
	cfg.GenesisNode = 7
	cfg.OldCodeSourceDir = src
	cfg.OldCodeNodes = []int{liar}
	cfg.OldCodeSysconfig = true
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnet(t, cfg, 90*time.Minute)

	liarAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, liar)
	targetAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, target)
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
	lie := func(on bool) {
		cmd := "printf %s " + targetAcct + " > /tmp/frame-equivocate"
		if !on {
			cmd = "rm -f /tmp/frame-equivocate"
		}
		out, err := exec.CommandContext(ctx, "docker", "exec", d.containerName(liar), "sh", "-c", cmd).CombinedOutput()
		if err != nil {
			t.Fatalf("switching the lie %v: %v %s", on, err, out)
		}
	}

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	keygen := waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
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
	t.Logf("1. baseline reshare held by all %d at %d (epoch %d); keyId=%s", cfg.Nodes, base.BlockHeight, base.Epoch, keygen.KeyId)

	// 2. One equivocated reshare.
	start, _ := getHeadBlock(d.HiveRPCEndpoint())
	lie(true)
	var eq *TssCommitmentDoc
	for eq == nil {
		r, err := d.WaitForCommitment(ctx, 2, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(start)}}, 10*time.Minute)
		if err != nil {
			lie(false)
			t.Fatalf("PRECONDITION FAILED: no reshare landed while %s equivocated: %v", liarAcct, err)
		}
		if contains(named(*r), targetAcct) && strings.Contains(grepLogs(liar, "FRAME-EQUIVOCATE"), targetAcct) {
			eq = r
		} else {
			start = int(r.BlockHeight)
		}
	}
	lie(false)
	stopped, _ := getHeadBlock(d.HiveRPCEndpoint())
	t.Logf("2. equivocated reshare landed at %d held by %v; lie off at %d (liar log: %s)", eq.BlockHeight, named(*eq), stopped, lastLine(grepLogs(liar, "FRAME-EQUIVOCATE")))

	// 3. Signing requests against the key.
	all := []int{1, 2, 3, 4, 5, 6, 7}
	for i := 0; i < 3; i++ {
		dg := sha256.Sum256([]byte(fmt.Sprintf("equiv-frame-%d-%d", stopped, i)))
		vfF13InsertRogueRequest(t, d, ctx, all, keygen.KeyId, dg[:])
	}
	t.Logf("3. inserted 3 signing requests for %s", keygen.KeyId)
	waitForBlock(t, d.HiveRPCEndpoint(), stopped+120, 10*time.Minute)

	// 4. What landed after the lie, and who got named.
	docs, _ := d.GetCommitments(ctx, 2, bson.M{"block_height": bson.M{"$gt": eq.BlockHeight}})
	blamesTarget, blamesLiar := 0, 0
	for _, c := range docs {
		who := named(c)
		t.Logf("after the lie: %-14s at %d epoch %d -> %v", c.Type, c.BlockHeight, c.Epoch, who)
		if c.Type == "blame" || c.Type == "reshare_accuse" {
			if contains(who, targetAcct) {
				blamesTarget++
			}
			if contains(who, liarAcct) {
				blamesLiar++
			}
		}
	}
	for _, n := range []int{3, 4} {
		t.Logf("magi-%d Bob_mid lines: %s", n, lastLine(grepLogs(n, "Bob_mid")))
		t.Logf("magi-%d exclusions of %s: %s", n, targetAcct, lastLine(grepLogs(n, "excluding (blamed|banned) node .*account="+targetAcct)))
		t.Logf("magi-%d ban lines for %s: %s", n, targetAcct, lastLine(grepLogs(n, "node banned.*account="+targetAcct)))
	}
	t.Logf("4. blame/accuse commitments naming honest %s=%d, naming liar %s=%d", targetAcct, blamesTarget, liarAcct, blamesLiar)

	if blamesTarget > 0 && blamesLiar == 0 {
		t.Errorf("FRAME-EQUIVOCATE: %d commitment(s) name the honest %s after %s equivocated one reshare; the liar is named in none", blamesTarget, targetAcct, liarAcct)
	} else {
		t.Logf("no framing observed (target named %d, liar named %d)", blamesTarget, blamesLiar)
	}
}
