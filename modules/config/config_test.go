package config_test

import (
	"config/config"
	"testing"
)

func TestBasic(t *testing.T) {
	type conf struct {
		A uint
		B string
	}
	c := config.New(conf{1, "hi"})
	err := c.Init()
	if err != nil {
		t.Fatal(err)
	}
	err = c.Start()
	if err != nil {
		t.Fatal(err)
	}
	err = c.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
