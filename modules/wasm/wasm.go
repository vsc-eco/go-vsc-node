package wasm

import (
	"fmt"
	a "vsc-node/modules/aggregate"

	"github.com/second-state/WasmEdge-go/wasmedge"
)

type Wasm struct {
}

var _ a.Plugin = &Wasm{}

func New() *Wasm {
	return &Wasm{}
}

func (w *Wasm) Init() error {
	err := setup()
	if err != nil {
		return err
	}
	return nil
}

func (w *Wasm) Start() error {
	return nil
}

func (w *Wasm) Stop() error {
	return nil
}

func (w *Wasm) Execute(byteCode []byte, gas uint, entrypoint string, args string) (string, error) {
	vm := wasmedge.NewVM()
	defer vm.Release()
	err := vm.RegisterWasmBuffer("contract", byteCode)
	if err != nil {
		return "", err
	}
	vm.GetStatistics().SetCostLimit(gas)
	res, err := vm.ExecuteRegistered("contract", entrypoint, args)
	if err != nil {
		return "", err
	}
	if len(res) != 1 {
		return "", fmt.Errorf("not exactly 1 return value")
	}
	switch v := res[0].(type) {
	case string:
		return v, nil
	}
	return "", fmt.Errorf("return value is not a string")
}
