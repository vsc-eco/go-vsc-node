package config_test

import (
	"context"
	"testing"
	"vsc-node/modules/config"
)

func TestBasic(t *testing.T) {
	type conf struct {
		A uint
		B string
	}
	c := config.New(conf{1, "hi"}, nil)
	err := c.Init()
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Start().Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = c.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
