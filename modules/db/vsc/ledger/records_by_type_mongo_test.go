package ledger_db

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
)

// GetLedgerRecordsByType feeds the per-epoch delegation and settlement readers,
// which fail-stop on an error. A record it cannot decode must surface as an
// error, not as a blank record in a result that reads as complete. Needs
// docker for an ephemeral MongoDB.
func TestGetLedgerRecordsByTypeReportsDecodeErrors(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const port = "47027"
	name := "vsc-ledger-bytype-mongo"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", port+":27017", "mongo:8.0.17").CombinedOutput(); err != nil {
		t.Skipf("could not start mongo container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	t.Setenv("MONGO_URL", "mongodb://localhost:"+port)

	conf := db.NewDbConfig(t.TempDir())
	if err := conf.Init(); err != nil {
		t.Fatalf("conf init: %v", err)
	}
	var (
		agg     aggregate.Plugin
		store   Ledger
		started bool
	)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		dbi := db.New(conf)
		vscDb := vsc.New(dbi, conf)
		store = New(vscDb)
		a := aggregate.New([]aggregate.Plugin{conf, dbi, vscDb, store})
		if err := a.Init(); err == nil {
			if _, err := a.Start().Await(context.Background()); err == nil {
				agg, started = a, true
				break
			}
		}
	}
	if !started {
		t.Fatal("mongo did not become ready in time")
	}
	t.Cleanup(func() { _ = agg.Stop() })

	l := store.(*ledger)
	if _, err := l.InsertOne(context.Background(), bson.M{"id": "good", "t": "consensus_stake", "block_height": 5, "amount": 100}); err != nil {
		t.Fatal(err)
	}
	if recs, err := store.GetLedgerRecordsByType([]string{"consensus_stake"}, 10); err != nil || len(recs) != 1 || recs[0].Amount != 100 {
		t.Fatalf("clean read: got %+v, %v", recs, err)
	}
	// amount must be an int64; a string cannot decode into the record.
	if _, err := l.InsertOne(context.Background(), bson.M{"id": "bad", "t": "consensus_stake", "block_height": 6, "amount": "not a number"}); err != nil {
		t.Fatal(err)
	}
	recs, err := store.GetLedgerRecordsByType([]string{"consensus_stake"}, 10)
	if err == nil {
		t.Fatalf("an undecodable record must be an error, got %d records and nil: %+v", len(recs), recs)
	}
}
