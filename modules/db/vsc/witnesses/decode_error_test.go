package witnesses_test

import (
	"context"
	"errors"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	"vsc-node/modules/db/vsc/witnesses"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// A stored witness row that does not decode is reported as db.ErrDecode by the
// reads the POA exit-halt and POA-1 locks make, so they hold instead of
// retrying forever.
func TestWitnessReadsMarkDecodeErrors(t *testing.T) {
	const dbName = "witnesses_decode_error_test"
	conf := db.NewDbConfig(t.TempDir())
	if err := conf.Init(); err != nil {
		t.Fatal(err)
	}
	if err := conf.SetDbName(dbName); err != nil {
		t.Fatal(err)
	}
	d := db.New(conf)
	vscDb := vsc.New(d, conf)
	wit := witnesses.New(vscDb)
	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{conf, d, vscDb, wit}))

	ctx := context.Background()
	cli, err := mongo.Connect(ctx, options.Client().ApplyURI(conf.Get().DbURI))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Disconnect(ctx)
	coll := cli.Database(dbName).Collection("witnesses")
	_ = coll.Drop(ctx)
	t.Cleanup(func() { _ = cli.Database(dbName).Drop(context.Background()) })
	if _, err := coll.InsertOne(ctx, bson.M{"account": "bad", "height": int64(10), "enabled": "yes"}); err != nil {
		t.Fatal(err)
	}

	if _, err := wit.GetWitnessesAtBlockHeight(20); !errors.Is(err, db.ErrDecode) {
		t.Fatalf("GetWitnessesAtBlockHeight: err=%v, want db.ErrDecode", err)
	}
	h := uint64(20)
	if _, err := wit.GetWitnessAtHeight("bad", &h); !errors.Is(err, db.ErrDecode) {
		t.Fatalf("GetWitnessAtHeight: err=%v, want db.ErrDecode", err)
	}
	if _, err := wit.GetWitnessAtHeight("absent", &h); errors.Is(err, db.ErrDecode) || !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("absent row: err=%v, want ErrNoDocuments and no ErrDecode", err)
	}
}
