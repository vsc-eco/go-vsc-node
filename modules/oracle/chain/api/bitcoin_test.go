package api

import (
	"os"
	"testing"
)

func TestBitcoinRelayer(t *testing.T) {
	if err := os.Setenv("DEBUG", "1"); err != nil {
		t.Fatal("failed to set DEBUG environment", err)
	}

	btc := &Bitcoin{}
	if err := btc.Init(t.Context()); err != nil {
		t.Fatal(err)
	}

	contractState, err := btc.GetContractState()
	if err != nil {
		t.Fatal(err)
	}

	t.Log("contractState", contractState)
}
