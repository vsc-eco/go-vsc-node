package graph_test

import (
	"testing"
	"vsc-node/modules/graph"
)

func Test(t *testing.T) {

	g := graph.New()

	err := g.Init()
	if err != nil {
		t.Fatal(err)
	}
	err = g.Start()
	if err != nil {
		t.Fatal(err)
	}
	err = g.Stop()
	if err != nil {
		t.Fatal(err)
	}

}
