package ledger_db

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"

	"go.mongodb.org/mongo-driver/bson"
)

// The byTxId filters are prefix matches built as {"$regex": "^" + txId}.
// txId comes straight from a GraphQL argument, so it used to be a regex: a
// malformed one answered with MongoDB's own error text ("(Location51091)
// Regular expression is invalid ...", reproduced on testnet 0.13.0), and a
// pattern like ".*" matched every record. It must be matched literally.
// Needs docker for an ephemeral MongoDB.
func TestTxIdFiltersMatchLiterally(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}
	const port = "47028"
	name := "vsc-ledger-txid-regex-mongo"
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
		actions BridgeActions
		started bool
	)
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(2 * time.Second) {
		dbi := db.New(conf)
		vscDb := vsc.New(dbi, conf)
		store = New(vscDb)
		actions = NewActionsDb(vscDb)
		a := aggregate.New([]aggregate.Plugin{conf, dbi, vscDb, store, actions})
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

	ctx := context.Background()
	const id = "bafyreiabc123-0"
	if _, err := store.(*ledger).InsertOne(ctx, bson.M{"id": id, "t": "transfer", "owner": "hive:a", "block_height": 5, "amount": 1}); err != nil {
		t.Fatal(err)
	}
	// GetLedgersTsRange joins hive_blocks for the timestamp and drops records
	// without a block, so give the record one.
	if _, err := store.(*ledger).Database().Collection("hive_blocks").InsertOne(ctx,
		bson.M{"block": bson.M{"block_number": 5, "timestamp": "2026-01-01T00:00:00"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := actions.(*actionsDb).InsertOne(ctx, bson.M{"id": id, "status": "complete", "block_height": 5}); err != nil {
		t.Fatal(err)
	}

	type lookup func(txId string) (int, error)
	lookups := map[string]lookup{
		"GetLedgersTsRange": func(s string) (int, error) {
			r, err := store.GetLedgersTsRange(nil, &s, nil, nil, nil, nil, 0, 10)
			return len(r), err
		},
		"GetRawLedgerRange": func(s string) (int, error) {
			r, err := store.GetRawLedgerRange(nil, &s, nil, nil, nil, nil, 0, 10)
			return len(r), err
		},
		"GetActionsByTxId": func(s string) (int, error) {
			r, err := actions.GetActionsByTxId(s)
			return len(r), err
		},
	}
	for name, get := range lookups {
		if n, err := get("bafyreiabc"); err != nil || n != 1 {
			t.Errorf("%s: prefix lookup got %d, %v; want 1 match", name, n, err)
		}
		if n, err := get(strings.Repeat("(", 41)); err != nil || n != 0 {
			t.Errorf("%s: malformed pattern got %d, %v; want 0 matches and no error", name, n, err)
		}
		if n, err := get(".*"); err != nil || n != 0 {
			t.Errorf("%s: \".*\" got %d, %v; want 0 matches (literal)", name, n, err)
		}
	}
}
