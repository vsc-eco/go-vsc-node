package wasm_test

import (
	"testing"
	"vsc-node/modules/wasm"
)

func TestCompat(t *testing.T) {
	w := wasm.New()
	err := w.Init()
	if err != nil {
		t.Fatal(err)
	}
	err = w.Start()
	if err != nil {
		t.Fatal(err)
	}
	err = w.Stop()
	if err != nil {
		t.Fatal(err)
	}
}
