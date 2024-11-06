package wasm_runtime_ipc_test

import (
	"testing"
	wasm_runtime_ipc "vsc-node/modules/wasm/runtime_ipc"
)

func TestCompat(t *testing.T) {
	w := wasm_runtime_ipc.New()
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
