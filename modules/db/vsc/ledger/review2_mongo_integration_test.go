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

// review2 HIGH #30 (actionsDb.SetStatus was an empty no-op) and #27 (no
// indexes on ledger_actions) both need a live MongoDB — the whole db/vsc
// suite is gated on one and there is none in CI/sandbox by default. This
// test spins an ephemeral mongo container so the real actionsDb code path
// is exercised.
//
// Differential:
//   - #170 baseline: SetStatus does nothing → status stays "pending" (RED);
//     no Init()/index on ledger_actions → only the default _id_ index (RED).
//   - fix/review2: SetStatus updates the doc → "complete" (GREEN); Init()
//     creates {status,block_height} and {id} indexes (GREEN).
func TestReview2LedgerActionsMongoIntegration(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not available")
	}

	const port = "47017" // fixed high port to keep teardown simple
	name := "vsc-review2-mongo"
	_ = exec.Command("docker", "rm", "-f", name).Run()
	run := exec.Command("docker", "run", "-d", "--rm", "--name", name,
		"-p", port+":27017", "mongo:8.0.17")
	if out, err := run.CombinedOutput(); err != nil {
		t.Skipf("could not start mongo container: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", name).Run() })

	uri := "mongodb://localhost:" + port
	t.Setenv("MONGO_URL", uri)

	// conf.Init() reads MONGO_URL; must run BEFORE db.New connects.
	conf := db.NewDbConfig()
	if err := conf.Init(); err != nil {
		t.Fatalf("conf init: %v", err)
	}

	// Wait for mongo to accept connections.
	dbi := db.New(conf)
	vscDb := vsc.New(dbi, conf)
	actions := NewActionsDb(vscDb)

	agg := aggregate.New([]aggregate.Plugin{conf, dbi, vscDb, actions})

	var started bool
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if err := agg.Init(); err == nil {
			if _, err := agg.Start().Await(context.Background()); err == nil {
				started = true
				break
			}
		}
		time.Sleep(2 * time.Second)
		dbi = db.New(conf)
		vscDb = vsc.New(dbi, conf)
		actions = NewActionsDb(vscDb)
		agg = aggregate.New([]aggregate.Plugin{conf, dbi, vscDb, actions})
	}
	if !started {
		t.Fatal("mongo did not become ready in time")
	}
	t.Cleanup(func() { _ = agg.Stop() })

	ctx := context.Background()
	coll := vscDb.Database.Collection("ledger_actions")

	// ---- #30: SetStatus must actually update the action ----
	t.Run("review2_30_SetStatus", func(t *testing.T) {
		const id = "review2-30-action"
		_, _ = coll.DeleteOne(ctx, bson.M{"id": id})
		actions.StoreAction(ActionRecord{Id: id, Status: "pending", Type: "withdraw", BlockHeight: 1})

		pre, err := actions.Get(id)
		if err != nil || pre == nil || pre.Status != "pending" {
			t.Fatalf("setup: stored action not found/pending: %+v err=%v", pre, err)
		}
		actions.SetStatus(id, "complete")
		post, err := actions.Get(id)
		if err != nil || post == nil {
			t.Fatalf("Get after SetStatus: %v", err)
		}
		if post.Status != "complete" {
			t.Fatalf("review2 #30: SetStatus did not update status (got %q, want \"complete\") — empty no-op", post.Status)
		}
	})

	// ---- #27: ledger_actions must carry the hot-query indexes ----
	t.Run("review2_27_Indexes", func(t *testing.T) {
		specs, err := coll.Indexes().ListSpecifications(ctx)
		if err != nil {
			t.Fatalf("list indexes: %v", err)
		}
		haveStatusBH, haveID := false, false
		for _, s := range specs {
			var keys bson.D
			if err := bson.Unmarshal(s.KeysDocument, &keys); err != nil {
				continue
			}
			fields := make([]string, 0, len(keys))
			for _, e := range keys {
				fields = append(fields, e.Key)
			}
			joined := strings.Join(fields, ",")
			if joined == "status,block_height" {
				haveStatusBH = true
			}
			if joined == "id" {
				haveID = true
			}
		}
		if !haveStatusBH || !haveID {
			var names []string
			for _, s := range specs {
				names = append(names, s.Name)
			}
			t.Fatalf("review2 #27: ledger_actions missing expected indexes "+
				"(status,block_height present=%v; id present=%v); have only: %v",
				haveStatusBH, haveID, names)
		}
	})
}
