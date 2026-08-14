package hive_blocks

import (
	"context"
	"fmt"
	"os/exec"
	"testing"
	"time"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// scrambled is deliberately non-monotonic in insertion order so that
// "storage order" and "block order" cannot coincide by accident.
var scrambled = []uint64{10, 7, 3, 9, 5, 1, 8, 2, 6, 4}

func startMongo(t *testing.T, port, name string) string {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	_ = exec.Command("docker", "rm", "-f", name).Run()
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", port+":27017", "mongo:8.0.17")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("could not start mongo: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	return "mongodb://localhost:" + port
}

func newHiveBlocks(t *testing.T, uri string) *hiveBlocks {
	t.Helper()
	t.Setenv("MONGO_URL", uri)

	conf := db.NewDbConfig()
	if err := conf.Init(); err != nil {
		t.Fatalf("conf init: %v", err)
	}
	dbi := db.New(conf)
	vscDb := vsc.New(dbi, conf)
	hb, err := New(vscDb)
	if err != nil {
		t.Fatalf("new hive_blocks: %v", err)
	}
	agg := aggregate.New([]aggregate.Plugin{conf, dbi, vscDb, hb.(*hiveBlocks)})

	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if lastErr = agg.Init(); lastErr == nil {
			break
		}
		time.Sleep(2 * time.Second)
	}
	if lastErr != nil {
		t.Skipf("mongo never became ready: %v", lastErr)
	}
	return hb.(*hiveBlocks)
}

func mkBlock(n uint64) HiveBlock {
	return HiveBlock{
		BlockNumber: n,
		BlockID:     fmt.Sprintf("block-%03d", n),
		Witness:     "initminer",
		Timestamp:   "2026-08-14T00:00:00",
		MerkleRoot:  fmt.Sprintf("mr-%03d", n),
	}
}

// TestListenToBlockUpdatesOrdering is the proof for the ListenToBlockUpdates
// sort fix. It runs against a real MongoDB because the whole question is what
// the SERVER returns, which no mock can answer.
//
// It asserts three things:
//
//  1. Storage order really is non-monotonic — an unsorted collection scan
//     returns blocks in insertion order, not block-number order. This is the
//     hazard the fix removes: without .SetSort() the driver imposes no order,
//     so the state engine can be fed blocks out of sequence.
//  2. The sorted query the fix installs returns strictly ascending order over
//     the exact same documents.
//  3. The real ListenToBlockUpdates delivers strictly ascending block numbers
//     end to end (happy path unchanged, ordering now guaranteed).
func TestListenToBlockUpdatesOrdering(t *testing.T) {
	uri := startMongo(t, "47019", "vsc-halt-hiveblocks-mongo")
	h := newHiveBlocks(t, uri)

	// Insert one at a time so insertion order == storage order.
	for _, n := range scrambled {
		if err := h.StoreBlocks(100, mkBlock(n)); err != nil {
			t.Fatalf("store %d: %v", n, err)
		}
	}

	ctx := context.Background()
	filter := bson.M{
		"type":               DocumentTypeHiveBlock,
		"block.block_number": bson.M{"$gt": uint64(0)},
	}

	collect := func(opts *options.FindOptions) []uint64 {
		cur, err := h.Collection.Collection.Find(ctx, filter, opts)
		if err != nil {
			t.Fatalf("find: %v", err)
		}
		defer cur.Close(ctx)
		var got []uint64
		for cur.Next(ctx) {
			var d Document
			if err := cur.Decode(&d); err != nil {
				t.Fatalf("decode: %v", err)
			}
			got = append(got, d.Block.BlockNumber)
		}
		return got
	}

	// (1) Unsorted collection scan — what an order-free Find is entitled to
	// return. $natural forces the storage order rather than an index walk.
	natural := collect(options.Find().SetHint(bson.D{{Key: "$natural", Value: 1}}))
	t.Logf("unsorted (storage/$natural) order: %v", natural)
	if len(natural) != len(scrambled) {
		t.Fatalf("expected %d blocks, got %d", len(scrambled), len(natural))
	}
	if isAscending(natural) {
		t.Errorf("storage order came back ascending (%v) — this test can no "+
			"longer distinguish sorted from unsorted; make the insert order "+
			"more adversarial", natural)
	}

	// (2) The sorted query the fix installs.
	sorted := collect(options.Find().SetSort(bson.D{{Key: "block.block_number", Value: 1}}))
	t.Logf("sorted (the fix) order:            %v", sorted)
	if !isAscending(sorted) {
		t.Fatalf("sorted query did not return ascending order: %v", sorted)
	}
	if len(sorted) != len(scrambled) {
		t.Fatalf("sorted query lost documents: got %d want %d", len(sorted), len(scrambled))
	}

	// (3) End to end through the real listener.
	listenCtx, listenCancel := context.WithCancel(context.Background())
	defer listenCancel()

	var delivered []uint64
	done := make(chan struct{})
	cancel, errChan := h.ListenToBlockUpdates(listenCtx, 1, func(b HiveBlock, head *uint64) error {
		delivered = append(delivered, b.BlockNumber)
		if len(delivered) == len(scrambled) {
			select {
			case <-done:
			default:
				close(done)
			}
		}
		return nil
	})
	defer cancel()
	// Drain errChan so the listener goroutine can never block on it.
	go func() {
		for range errChan {
		}
	}()

	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatalf("listener only delivered %d/%d blocks: %v",
			len(delivered), len(scrambled), delivered)
	}
	cancel()

	t.Logf("ListenToBlockUpdates delivered:    %v", delivered)
	if !isAscending(delivered) {
		t.Fatalf("listener delivered OUT OF ORDER: %v", delivered)
	}
	if len(delivered) != len(scrambled) {
		t.Fatalf("listener delivered %d blocks, want %d", len(delivered), len(scrambled))
	}
}

func isAscending(v []uint64) bool {
	for i := 1; i < len(v); i++ {
		if v[i] <= v[i-1] {
			return false
		}
	}
	return true
}
