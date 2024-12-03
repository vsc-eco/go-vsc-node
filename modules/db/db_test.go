package db_test

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"vsc-node/modules/aggregate"
	"vsc-node/modules/db"

	"go.mongodb.org/mongo-driver/mongo/options"
)

func clearDbTestFiles() {
	os.RemoveAll("data/db.sqlite")
	os.RemoveAll("data/db.sqlite-shm")
	os.RemoveAll("data/db.sqlite-wal")

}

// cleans the data directory after the test is done to
// prevent against (potential) unbounded growth issues
// for test dbs
func setupAndCleanUpDataDir(t *testing.T) {

	t.Cleanup(func() { clearDbTestFiles() })

	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-signalChan
		clearDbTestFiles()
		os.Exit(1)
	}()
}

type doc struct {
	Name string
}

type empty struct{}

func TestCompat(t *testing.T) {
	setupAndCleanUpDataDir(t)
	conf := db.NewDbConfig()
	d := db.New(conf)
	agg := aggregate.New([]aggregate.Plugin{
		conf,
		d,
	})
	err := agg.Init()
	if err != nil {
		t.Fatal(err)
	}
	_, err = agg.Start().Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := d.Database("db").Collection("col")
	_, err = c.InsertOne(context.TODO(), doc{"A"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.InsertOne(context.TODO(), doc{"B"})
	if err != nil {
		t.Fatal(err)
	}
	opts := options.Find()
	opts.SetSkip(1)
	cur, err := c.Find(context.TODO(), empty{}, opts)
	if err != nil {
		t.Fatal(err)
	}
	res := make([]doc, 1)
	err = cur.All(context.TODO(), &res)
	if err != nil {
		t.Fatal(err)
	}
	if res[0].Name != "B" {
		t.Fatal("skip does not work")
	}
	err = agg.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
