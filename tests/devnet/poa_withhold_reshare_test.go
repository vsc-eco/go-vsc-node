package devnet

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// latestElection returns the highest election epoch in a node's DB and the block it was stored at.
func latestElection(t *testing.T, d *Devnet, ctx context.Context, node int) (uint64, uint64) {
	client, err := d.mongoClient(ctx)
	if err != nil {
		return 0, 0
	}
	defer client.Disconnect(ctx)
	var e struct {
		Epoch       uint64 `bson:"epoch"`
		BlockHeight uint64 `bson:"block_height"`
	}
	if err := client.Database(d.nodeDbName(node)).Collection("elections").FindOne(ctx, bson.M{},
		options.FindOne().SetSort(bson.M{"epoch": -1})).Decode(&e); err != nil {
		return 0, 0
	}
	return e.Epoch, e.BlockHeight
}

// TestPoaSeatAttestsAndWithholdsEverySession (POA-8): one seat is online for every
// readiness round (reconnected 10 blocks before each boundary, so it attests) and
// silent for every reshare session (disconnected 3 blocks before it). Before 0.9.0
// no blame naming it could land (honest nodes' culprit sets differ, and from 0.8.0
// the timeout set is withheld), so the seat stalled key rotation for as long as it
// kept this up. At 0.9.0 per-accused statements name it and the next reshare runs
// without it: at least one reshare must land inside the adversary window.
// Positive control: once the seat stops attesting, a reshare without it lands.
func TestPoaSeatAttestsAndWithholdsEverySession(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping devnet integration test in short mode")
	}
	cfg := tssTestConfig()
	cp := cfg.SysConfigOverrides.ConsensusParams
	cp.ConsensusVersionFloorMajor = 0
	cp.ConsensusVersionFloorConsensus = poaDevnetFloor()
	cp.ConsensusVersionFloorEpoch = 1
	d, ctx := startDevnet(t, cfg, 70*time.Minute)

	const silent = 3
	silentAcct := fmt.Sprintf("%s%d", cfg.WitnessPrefix, silent)
	var cycles []int
	t.Cleanup(func() {
		dctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		all, _ := d.GetCommitments(dctx, 1, bson.M{})
		for i := len(all) - 1; i >= 0; i-- {
			c := all[i]
			t.Logf("commitment %-8s block=%d epoch=%d bits=%s", c.Type, c.BlockHeight, c.Epoch, decodeBitset(t, c.Commitment).Text(2))
		}
		for n := 1; n <= cfg.Nodes; n++ {
			out, _ := exec.CommandContext(dctx, "bash", "-c", fmt.Sprintf("docker logs %s 2>&1 | grep -E 'timeout result|suppressing systemic|collecting sigs|waitForSigs (OK|failed|timeout)|reshare accusations|accusation exclusions|excluding accused' | tail -80", d.containerName(n))).CombinedOutput()
			t.Logf("magi-%d:\n%s", n, string(out))
		}
	})

	for n := 1; n <= cfg.Nodes; n++ {
		if err := d.waitForElectionEpoch(ctx, n, 2, 10*time.Minute); err != nil {
			t.Fatalf("magi-%d never reached epoch 2: %v", n, err)
		}
	}
	waitForCommitment(t, d.MongoURI(), "keygen", 15*time.Minute)
	head0, _ := getHeadBlock(d.HiveRPCEndpoint())
	base, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(head0)}}, 10*time.Minute)
	if err != nil {
		t.Fatalf("PRECONDITION FAILED: no baseline reshare with all 5 online: %v", err)
	}
	t.Logf("baseline reshare with all 5 at %d (epoch %d)", base.BlockHeight, base.Epoch)

	// Wait for the first epoch change after the baseline, then run the adversary for 8 boundaries.
	var first int
	for tries := 0; first == 0; tries++ {
		if tries > 400 {
			t.Fatalf("PRECONDITION FAILED: no epoch change after the baseline")
		}
		head, _ := getHeadBlock(d.HiveRPCEndpoint())
		ep, eh := latestElection(t, d, ctx, 1)
		b := nextReshareBoundary(head)
		if ep > base.Epoch && b-head >= 6 && eh <= uint64(head) {
			first = b
			t.Logf("election epoch %d > key epoch %d at head %d; adversary starts at boundary %d", ep, base.Epoch, head, b)
		}
		time.Sleep(3 * time.Second)
	}
	for b := first; b < first+8*testRotateInterval; b += testRotateInterval {
		if b != first {
			waitForBlock(t, d.HiveRPCEndpoint(), b-10, 3*time.Minute)
			if err := d.Reconnect(ctx, silent); err != nil {
				t.Logf("reconnect before %d: %v", b, err)
			}
		}
		waitForBlock(t, d.HiveRPCEndpoint(), b-3, 3*time.Minute)
		if err := d.Disconnect(ctx, silent); err != nil {
			t.Fatalf("disconnect before %d: %v", b, err)
		}
		cycles = append(cycles, b)
		t.Logf("cycle %d: %s online for readiness, silent from %d", b, silentAcct, b-3)
	}
	last := cycles[len(cycles)-1]
	// Let the last session time out, then count what landed during the adversary window.
	waitForBlock(t, d.HiveRPCEndpoint(), last+45, 5*time.Minute)
	win := bson.M{"block_height": bson.M{"$gte": uint64(first), "$lte": uint64(last)}}
	docs, _ := d.GetCommitments(ctx, 1, win)
	reshares, blames, withSilent, accusedSilent := 0, 0, 0, 0
	for _, c := range docs {
		mem, _ := d.GetElectionMembers(ctx, 1, c.Epoch)
		bits := decodeBitset(t, c.Commitment)
		var named []string
		for j, m := range mem {
			if bits.Bit(j) == 1 {
				named = append(named, strings.TrimPrefix(m, "hive:"))
			}
		}
		t.Logf("in window: %s at %d epoch %d -> %v", c.Type, c.BlockHeight, c.Epoch, named)
		switch c.Type {
		case "reshare":
			reshares++
		case "blame":
			blames++
			for _, a := range named {
				if a == silentAcct {
					withSilent++
				}
			}
		case "reshare_accuse": // POA-8 (0.9.0) per-accused statement
			for _, a := range named {
				if a == silentAcct {
					accusedSilent++
				}
			}
		}
	}
	t.Logf("adversary window %d..%d (%d sessions, floor 0.%d): reshares=%d blames=%d (naming %s: %d) accusations naming %s=%d", first, last, len(cycles), poaDevnetFloor(), reshares, blames, silentAcct, withSilent, silentAcct, accusedSilent)

	// Positive control: stop attesting (stay disconnected) -> a reshare without the seat must land.
	rec, err := d.WaitForCommitment(ctx, 1, bson.M{"type": "reshare", "block_height": bson.M{"$gt": uint64(last)}}, 8*time.Minute)
	if err != nil {
		t.Errorf("CONTROL FAILED: no reshare within 8 min after the seat stopped attesting: %v", err)
	} else {
		t.Logf("control: reshare at %d once %s stopped attesting", rec.BlockHeight, silentAcct)
	}
	switch {
	case reshares == 0 && withSilent == 0 && accusedSilent == 0:
		t.Errorf("a single seat that attests ready and then withholds stalled key rotation for all %d sessions (%d..%d): no reshare and no blame naming it landed", len(cycles), first, last)
	case withSilent > 0:
		t.Logf("PASS (blame works): %d blame(s) named the withholding seat inside the window", withSilent)
	case reshares == 0:
		t.Errorf("%d accusation(s) named the withholding seat but no reshare landed inside the window", accusedSilent)
	default:
		t.Logf("PASS (no stall): %d reshare(s) landed inside the window despite the adversary", reshares)
	}
}
