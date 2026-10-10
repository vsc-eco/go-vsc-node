package db

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/bson"
)

// A DB left by a binary that records no processed_under metadata (the current
// mainnet release) must be replayed only when that binary kept processing past a
// version rise it could not follow. Needs docker for an ephemeral MongoDB.
func TestReindexDecisionForDbWithoutVersionMetadata(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const port = "47023"
	name := "vsc-reindex-mongo"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", port+":27017", "mongo:8.0.17").CombinedOutput(); err != nil {
		t.Skipf("could not start mongo container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	t.Setenv("MONGO_URL", "mongodb://localhost:"+port)

	conf := NewDbConfig(t.TempDir()) // keep the test URL out of the package data dir
	if err := conf.Init(); err != nil {
		t.Fatalf("conf init: %v", err)
	}
	client := New(conf)
	var err error
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		if err = client.Init(); err == nil {
			break
		}
	}
	if err != nil {
		t.Fatalf("mongo did not become ready: %v", err)
	}
	t.Cleanup(func() { _ = client.Stop() })

	ctx := context.Background()
	// run seeds a DB as the old binary leaves it (reindex_id current, a last
	// processed block, no processed_under), then runs the reindex gate with the
	// release binary (0.9) against a chain whose active version at that block
	// is chainCons. It reports whether the derived state was dropped.
	run := func(chainCons uint64) bool {
		inst := NewDbInstance(client, conf)
		if err := inst.Init(); err != nil {
			t.Fatalf("instance init: %v", err)
		}
		if err := inst.Database.Drop(ctx); err != nil {
			t.Fatalf("drop: %v", err)
		}
		last := uint64(5000)
		if _, err := inst.Collection("hive_blocks").InsertOne(ctx, bson.M{
			"type": "metadata", "reindex_id": REINDEX_ID, "last_processed_block": last,
		}); err != nil {
			t.Fatalf("seed metadata: %v", err)
		}
		if _, err := inst.Collection("ledger").InsertOne(ctx, bson.M{"id": "derived-row"}); err != nil {
			t.Fatalf("seed derived state: %v", err)
		}
		gate := NewReindex(inst, false, &VersionReindex{
			RunningMajor:     0,
			RunningConsensus: 9,
			ChainActiveAt: func(uint64) (uint64, uint64, bool) {
				return 0, chainCons, true
			},
		})
		if err := gate.Init(); err != nil {
			t.Fatalf("reindex init: %v", err)
		}
		n, err := inst.Collection("ledger").CountDocuments(ctx, bson.M{})
		if err != nil {
			t.Fatalf("count: %v", err)
		}
		return n == 0
	}

	if run(3) {
		t.Fatal("old binary upgraded before the rise (chain at 0.3): derived state was dropped, an unneeded full replay")
	}
	if !run(9) {
		t.Fatal("old binary ran past the 0.9 rise: derived state was kept, the node stays on state applied under old rules")
	}
}
