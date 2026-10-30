package nonces

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/mongo"
)

// SetNonce reports its write result. The first write for an account is an
// upsert that inserts, which must succeed (the driver reports the missing prior
// document as ErrNoDocuments); an overwrite must succeed; GetNonce reads both
// back, and an account never written is ErrNoDocuments. A dropped write would
// otherwise diverge that node once apply reads the nonce. Needs docker for an
// ephemeral MongoDB.
func TestSetNonceReportsWrites(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const port = "47022"
	name := "vsc-nonce-store-mongo"
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
		store   Nonces
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

	if err := store.SetNonce("key-a", 1); err != nil {
		t.Fatalf("first write (upsert insert): %v", err)
	}
	if rec, err := store.GetNonce("key-a"); err != nil || rec.Nonce != 1 {
		t.Fatalf("GetNonce after insert = %+v, %v; want 1", rec, err)
	}
	if err := store.SetNonce("key-a", 5); err != nil {
		t.Fatalf("overwrite: %v", err)
	}
	if rec, err := store.GetNonce("key-a"); err != nil || rec.Nonce != 5 {
		t.Fatalf("GetNonce after overwrite = %+v, %v; want 5", rec, err)
	}
	// The apply-side nonce read treats exactly this error as nonce 0 and retries
	// any other, so a different not-found error would stall every new account.
	if _, err := store.GetNonce("never-written"); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("GetNonce for an account never written: want ErrNoDocuments, got %v", err)
	}
}
