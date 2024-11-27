package gql_test

import (
	"context"
	"testing"
	"vsc-node/modules/gql"
)

func Test(t *testing.T) {

	g := gql.New()

	err := g.Init()
	if err != nil {
		t.Fatal(err)
	}
	_, err = g.Start().Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	select {}

	err = g.Stop()
	if err != nil {
		t.Fatal(err)
	}

}
