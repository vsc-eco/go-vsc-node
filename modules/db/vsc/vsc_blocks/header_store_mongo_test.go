package vscBlocks

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/mongo"
)

// StoreHeader reports its write result: an insert and an update both succeed,
// and a second header for an already-filled slot is a duplicate-key error (a
// dropped write would otherwise diverge that node). Needs docker for an
// ephemeral MongoDB.
func TestHeaderStoreReportsWrites(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const port = "47021"
	name := "vsc-header-store-mongo"
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
		blocks  VscBlocks
		started bool
	)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		dbi := db.New(conf)
		vscDb := vsc.New(dbi, conf)
		blocks = New(vscDb)
		a := aggregate.New([]aggregate.Plugin{conf, dbi, vscDb, blocks})
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

	if err := blocks.StoreHeader(VscHeaderRecord{Id: "tx-a", SlotHeight: 100}); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if err := blocks.StoreHeader(VscHeaderRecord{Id: "tx-a", SlotHeight: 100, EndBlock: 7}); err != nil {
		t.Fatalf("update of the same header: %v", err)
	}
	h, err := blocks.GetBlockByHeight(100)
	if err != nil || h == nil || h.Id != "tx-a" || h.EndBlock != 7 {
		t.Fatalf("GetBlockByHeight(100) = %+v err=%v", h, err)
	}
	err = blocks.StoreHeader(VscHeaderRecord{Id: "tx-b", SlotHeight: 100})
	if !mongo.IsDuplicateKeyError(err) {
		t.Fatalf("second header for a filled slot: want a duplicate-key error, got %v", err)
	}
}
