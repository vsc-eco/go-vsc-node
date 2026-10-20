package devnet

// A/B check for Config.FastBlocks (pacemaker.go). Skipped unless FASTBLOCKS is set.
//
// The same network both ways: tssTestConfig, timeouts included. The chain runs to head
// TRACE_HEAD (default 200), then on past it so every ceremony started at or
// below that head has landed, and the test records, for every node, each
// election (epoch@block and its members), TSS commitment (type/epoch@block and
// its party bitset) and L2 block slot at or below TRACE_HEAD, plus wall clock and
// pacemaker counts. All nodes must agree within
// a run; the TRACE line is then diffed between the two runs.
//
//	FASTBLOCKS=0 go test -run TestFastBlocksTrace -v -timeout 60m ./tests/devnet/
//	FASTBLOCKS=1 go test -run TestFastBlocksTrace -v -timeout 60m ./tests/devnet/

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

func TestFastBlocksTrace(t *testing.T) {
	mode := os.Getenv("FASTBLOCKS")
	if testing.Short() || (mode != "0" && mode != "1") {
		t.Skip("A/B check: run with FASTBLOCKS=0 (real cadence) or FASTBLOCKS=1")
	}
	fast := mode == "1"
	target := 200
	if v, err := strconv.Atoi(os.Getenv("TRACE_HEAD")); err == nil && v > 0 {
		target = v
	}
	// Ceremonies started at or below target land within a few blocks; run this far
	// past it before reading so none is cut off.
	const settle = 40

	// tssTestConfig's own TSS timeouts (2m reshare). With DevnetTssParams' 20s, a
	// node still computing reshare round 2 (8-18s here, under the party lock, so
	// lastMsg does not move) times out at either cadence, and the trace measures
	// that noise instead of the cadence.
	cfg := tssTestConfig()
	cfg.FastBlocks = fast

	t.Logf("mode fast=%v target head=%d settle=+%d", fast, target, settle)
	wall := time.Now() // wall: measures the run
	d, ctx := startDevnet(t, cfg, 55*time.Minute)
	booted := time.Since(wall) // wall: measures the run
	// TRACE_LOG_DIR keeps every node's log (registered after startDevnet, so it
	// runs before the teardown, pass or fail).
	if dir := os.Getenv("TRACE_LOG_DIR"); dir != "" {
		t.Cleanup(func() {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Logf("TRACE_LOG_DIR: %v", err)
				return
			}
			for n := 1; n <= cfg.Nodes; n++ {
				logs, err := d.Logs(context.Background(), fmt.Sprintf("magi-%d", n))
				if err != nil {
					t.Logf("magi-%d logs: %v", n, err)
				}
				if werr := os.WriteFile(filepath.Join(dir, fmt.Sprintf("magi-%d.log", n)), []byte(logs), 0o644); werr != nil {
					t.Logf("magi-%d logs: %v", n, werr)
				}
			}
		})
	}
	rpc := d.HiveRPCEndpoint()

	startHead, _ := getHeadBlock(rpc)
	sampler := startReadLagSampler(d)
	waitForBlock(t, rpc, target, 45*time.Minute)
	reached := time.Since(wall) // wall: measures the run
	waitForBlock(t, rpc, target+settle, 10*time.Minute)
	settled := time.Since(wall) // wall: measures the run
	readLag := sampler.report(uint64(startHead)+10, uint64(target+settle))

	client, err := d.mongoClient(ctx)
	if err != nil {
		t.Fatalf("mongo: %v", err)
	}
	defer client.Disconnect(ctx)

	traces := make([]string, cfg.Nodes+1)
	for n := 1; n <= cfg.Nodes; n++ {
		db := client.Database(d.nodeDbName(n))

		var elections []struct {
			Epoch       uint64 `bson:"epoch"`
			BlockHeight uint64 `bson:"block_height"`
			Members     []struct {
				Account string `bson:"account"`
			} `bson:"members"`
		}
		cur, err := db.Collection("elections").Find(ctx,
			bson.M{"block_height": bson.M{"$lte": target}},
			options.Find().SetSort(bson.D{{Key: "epoch", Value: 1}}))
		if err != nil || cur.All(ctx, &elections) != nil {
			t.Fatalf("magi-%d elections: %v", n, err)
		}

		var commits []struct {
			Type        string `bson:"type"`
			KeyId       string `bson:"key_id"`
			Epoch       uint64 `bson:"epoch"`
			BlockHeight uint64 `bson:"block_height"`
			Commitment  string `bson:"commitment"`
		}
		cur, err = db.Collection("tss_commitments").Find(ctx,
			bson.M{"block_height": bson.M{"$lte": target}},
			options.Find().SetSort(bson.D{{Key: "block_height", Value: 1}, {Key: "type", Value: 1}, {Key: "key_id", Value: 1}}))
		if err != nil || cur.All(ctx, &commits) != nil {
			t.Fatalf("magi-%d commitments: %v", n, err)
		}

		var blocks []struct {
			SlotHeight uint64 `bson:"slot_height"`
		}
		cur, err = db.Collection("block_headers").Find(ctx,
			bson.M{"slot_height": bson.M{"$lte": target}},
			options.Find().SetSort(bson.D{{Key: "slot_height", Value: 1}}))
		if err != nil || cur.All(ctx, &blocks) != nil {
			t.Fatalf("magi-%d L2 blocks: %v", n, err)
		}

		var b strings.Builder
		b.WriteString("E:")
		for _, e := range elections {
			accounts := make([]string, len(e.Members))
			for i, m := range e.Members {
				accounts[i] = m.Account
			}
			fmt.Fprintf(&b, " e%d@%d[%s]", e.Epoch, e.BlockHeight, strings.Join(accounts, ","))
		}
		b.WriteString(" | C:")
		for _, c := range commits {
			fmt.Fprintf(&b, " %s/%s/e%d@%d[%s]", c.Type, c.KeyId, c.Epoch, c.BlockHeight, c.Commitment)
		}
		b.WriteString(" | B:")
		for _, blk := range blocks {
			fmt.Fprintf(&b, " %d", blk.SlotHeight)
		}
		traces[n] = b.String()
	}

	for n := 2; n <= cfg.Nodes; n++ {
		if traces[n] != traces[1] {
			t.Errorf("magi-%d trace differs from magi-1:\n  magi-1: %s\n  magi-%d: %s", n, traces[1], n, traces[n])
		}
	}
	assertNoSSIDMismatch(t, d, ctx, cfg.Nodes)

	early, capped, safe := d.PacemakerStats()
	t.Logf("TRACE fast=%v target=%d %s", fast, target, traces[1])
	t.Logf("TIMING fast=%v boot=%s startHead=%d reach%d=%s settle%d=%s chainPart=%s pacemaker early=%d capped=%d safe=%v",
		fast, booted.Round(time.Second), startHead, target, reached.Round(time.Second),
		target+settle, settled.Round(time.Second), (settled - booted).Round(time.Second), early, capped, safe)
	t.Logf("READLAG fast=%v %s", fast, readLag)
	if fast {
		// A fast run that silently ran at real cadence is not a fast run.
		if early == 0 {
			t.Errorf("FastBlocks produced no early block: the pacemaker never sped anything up")
		}
		if safe {
			t.Errorf("pacemaker fell back to safe mode; see the INTEGRITY FAILURE log lines")
		}
	}
}
