package tss_db_test

import (
	"context"
	"errors"
	"testing"

	"vsc-node/lib/test_utils"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"
	"vsc-node/modules/db/vsc"
	tss_db "vsc-node/modules/db/vsc/tss"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// A stored commitment row that does not decode is reported as db.ErrDecode,
// so the POA-1 lock holds instead of retrying forever; a full scan over more
// rows than one cursor batch still returns every row.
func TestFindCommitmentsSimpleMarksDecodeErrors(t *testing.T) {
	const dbName = "tss_commitments_decode_error_test"
	conf := db.NewDbConfig(t.TempDir())
	if err := conf.Init(); err != nil {
		t.Fatal(err)
	}
	if err := conf.SetDbName(dbName); err != nil {
		t.Fatal(err)
	}
	d := db.New(conf)
	vscDb := vsc.New(d, conf)
	commits := tss_db.NewCommitments(vscDb)
	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{conf, d, vscDb, commits}))

	ctx := context.Background()
	cli, err := mongo.Connect(ctx, options.Client().ApplyURI(conf.Get().DbURI))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Disconnect(ctx)
	coll := cli.Database(dbName).Collection("tss_commitments")
	_, _ = coll.DeleteMany(ctx, bson.M{})
	t.Cleanup(func() { _ = cli.Database(dbName).Drop(context.Background()) })

	key := "k-main"
	docs := make([]any, 0, 300)
	for h := 1; h <= 300; h++ { // more than the driver's first batch (101)
		docs = append(docs, bson.M{"key_id": key, "type": "reshare", "block_height": int64(h), "epoch": int64(h), "commitment": "AQ", "tx_id": "t"})
	}
	if _, err := coll.InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	below := uint64(1000)
	rows, err := commits.FindCommitmentsSimple(&key, []string{"keygen", "reshare"}, nil, nil, &below, 0)
	if err != nil || len(rows) != 300 {
		t.Fatalf("full scan: %d rows, err=%v, want 300", len(rows), err)
	}

	if _, err := coll.InsertOne(ctx, bson.M{"key_id": key, "type": "reshare", "block_height": int64(500), "epoch": "not-a-number", "commitment": "AQ", "tx_id": "bad"}); err != nil {
		t.Fatal(err)
	}
	if _, err := commits.FindCommitmentsSimple(&key, []string{"keygen", "reshare"}, nil, nil, &below, 0); !errors.Is(err, db.ErrDecode) {
		t.Fatalf("err=%v, want db.ErrDecode", err)
	}
}

// GetCommitmentByHeight marks a newest row that does not decode with
// db.ErrDecode (POA-1's C2 path reads it): unmarked, the consensus caller
// classed it transient and retried the unstake forever on every node.
func TestGetCommitmentByHeightMarksDecodeErrors(t *testing.T) {
	const dbName = "tss_commitments_by_height_decode_test"
	conf := db.NewDbConfig(t.TempDir())
	if err := conf.Init(); err != nil {
		t.Fatal(err)
	}
	if err := conf.SetDbName(dbName); err != nil {
		t.Fatal(err)
	}
	d := db.New(conf)
	vscDb := vsc.New(d, conf)
	commits := tss_db.NewCommitments(vscDb)
	test_utils.RunPlugin(t, aggregate.New([]aggregate.Plugin{conf, d, vscDb, commits}))

	ctx := context.Background()
	cli, err := mongo.Connect(ctx, options.Client().ApplyURI(conf.Get().DbURI))
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Disconnect(ctx)
	coll := cli.Database(dbName).Collection("tss_commitments")
	_, _ = coll.DeleteMany(ctx, bson.M{})
	t.Cleanup(func() { _ = cli.Database(dbName).Drop(context.Background()) })

	key := "evm-primary"
	if _, err := commits.GetCommitmentByHeight(key, 100, "reshare", "keygen"); !errors.Is(err, mongo.ErrNoDocuments) {
		t.Fatalf("no row: err=%v, want mongo.ErrNoDocuments", err)
	}
	if _, err := coll.InsertOne(ctx, bson.M{"key_id": key, "type": "keygen", "block_height": int64(10), "epoch": int64(2), "commitment": "AQ", "tx_id": "t"}); err != nil {
		t.Fatal(err)
	}
	if c, err := commits.GetCommitmentByHeight(key, 100, "reshare", "keygen"); err != nil || c.Epoch != 2 {
		t.Fatalf("good row: %+v err=%v", c, err)
	}
	if _, err := coll.InsertOne(ctx, bson.M{"key_id": key, "type": "reshare", "block_height": int64(20), "epoch": "not-a-number", "commitment": "AQ", "tx_id": "bad"}); err != nil {
		t.Fatal(err)
	}
	if _, err := commits.GetCommitmentByHeight(key, 100, "reshare", "keygen"); !errors.Is(err, db.ErrDecode) {
		t.Fatalf("undecodable newest row: err=%v, want db.ErrDecode", err)
	}
}
