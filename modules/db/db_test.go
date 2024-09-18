package db_test

import (
	"context"
	"testing"
	"vsc-node/modules/db"

	"go.mongodb.org/mongo-driver/mongo/options"
)

type doc struct {
	Name string
}

type empty struct{}

func TestCompat(t *testing.T) {
	d := db.New()
	err := d.Init()
	if err != nil {
		t.Fatal(err)
	}
	err = d.Start()
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
	err = d.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
