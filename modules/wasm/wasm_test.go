package wasm_test

import (
	"context"
	"testing"
	"vsc-node/modules/wasm"
)

func TestCompat(t *testing.T) {
	w := wasm.New()
	err := w.Init()
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.Start().Await(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	err = w.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
