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

// A transaction a block included keeps its anchor and status when a gossip copy
// is ingested into the pool afterwards. Needs docker for an ephemeral MongoDB.
func TestPoolIngestKeepsBlockAnchor(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const port = "47025"
	name := "vsc-anchor-mongo"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	if out, err := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", port+":27017", "mongo:8.0.17").CombinedOutput(); err != nil {
		t.Skipf("could not start mongo container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })
	t.Setenv("MONGO_URL", "mongodb://localhost:"+port)

	conf := db.NewDbConfig(t.TempDir()) // keep the test URL out of the package data dir
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

	anchor, height, index, block := "l1-produce-block-tx", uint64(777), int64(2), "hive-block-id"
	if err := txDb.Ingest(IngestTransactionUpdate{
		Id: "anchored-tx", Status: "INCLUDED", Type: "vsc", RequiredAuths: []string{"hive:alice"},
		Nonce: 1, RcLimit: 100, AnchoredId: &anchor, AnchoredHeight: &height, AnchoredIndex: &index, AnchoredBlock: &block,
	}); err != nil {
		t.Fatalf("block ingest: %v", err)
	}
	// A gossip copy reaching the pool afterwards: no anchor, no status.
	if err := txDb.Ingest(IngestTransactionUpdate{
		Id: "anchored-tx", Type: "vsc", RequiredAuths: []string{"hive:alice"}, Nonce: 1, RcLimit: 100,
	}); err != nil {
		t.Fatalf("pool ingest: %v", err)
	}
	rec := txDb.GetTransaction("anchored-tx")
	if rec == nil {
		t.Fatal("transaction not found")
	}
	if rec.Status != TransactionStatusIncluded {
		t.Fatalf("status changed to %q", rec.Status)
	}
	if rec.AnchoredId == nil || *rec.AnchoredId != anchor || rec.AnchoredHeight != height || rec.AnchoredIndex != index || rec.AnchoredBlock != block {
		t.Fatalf("the pool ingest erased the block anchor: id=%v height=%d index=%d block=%q", rec.AnchoredId, rec.AnchoredHeight, rec.AnchoredIndex, rec.AnchoredBlock)
	}
}
