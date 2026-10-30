package transactions

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
)

// GetTransaction returns nil both for a missing record and for a failed read,
// so a caller cannot tell them apart. GetTransactionErr separates them: (nil,
// nil) when the record does not exist, an error when the read failed. Needs
// docker for an ephemeral MongoDB.
func TestGetTransactionErrSeparatesMissingFromFailure(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const port = "47026"
	name := "vsc-gettx-err-mongo"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", port+":27017", "mongo:8.0.17").CombinedOutput(); err != nil {
		t.Skipf("could not start mongo container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	t.Setenv("MONGO_URL", "mongodb://localhost:"+port+"/?serverSelectionTimeoutMS=2000")

	conf := db.NewDbConfig(t.TempDir())
	if err := conf.Init(); err != nil {
		t.Fatalf("conf init: %v", err)
	}
	var (
		agg     aggregate.Plugin
		txDb    Transactions
		started bool
	)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		dbi := db.New(conf)
		vscDb := vsc.New(dbi, conf)
		txDb = New(vscDb)
		a := aggregate.New([]aggregate.Plugin{conf, dbi, vscDb, txDb})
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

	if err := txDb.Ingest(IngestTransactionUpdate{
		Id: "tx-a", Status: "CONFIRMED", Type: "vsc", RequiredAuths: []string{"hive:alice"}, Nonce: 1, RcLimit: 100,
	}); err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if rec, err := txDb.GetTransactionErr("tx-a"); err != nil || rec == nil || rec.Id != "tx-a" {
		t.Fatalf("found: got %+v, %v", rec, err)
	}
	if rec, err := txDb.GetTransactionErr("missing"); err != nil || rec != nil {
		t.Fatalf("missing: want (nil, nil), got %+v, %v", rec, err)
	}

	if out, err := exec.Command("docker", "stop", name).CombinedOutput(); err != nil {
		t.Fatalf("stop mongo: %v: %s", err, out)
	}
	if rec := txDb.GetTransaction("tx-a"); rec != nil {
		t.Fatalf("GetTransaction with the DB down: got %+v", rec)
	}
	if _, err := txDb.GetTransactionErr("tx-a"); err == nil {
		t.Fatal("GetTransactionErr with the DB down: want an error, got nil (indistinguishable from a missing record)")
	}
}
