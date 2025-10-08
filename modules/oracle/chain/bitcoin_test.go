package chain

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBitcoinRelayer(t *testing.T) {
	if err := os.Setenv("DEBUG", "1"); err != nil {
		t.Fatal("failed to set DEBUG environment", err)
	}

	b := &bitcoinRelayer{}

	assert.NoError(t, b.Init())

	chainState, err := b.TickCheck()
	if err != nil {
		t.Fatal("failed TickCheck", err)
	}

	t.Log("TickCheck chainState", chainState)

	chainDataBytes, err := b.ChainData(chainState)
	if err != nil {
		t.Fatal("failed ChainData", err)
	}
	t.Log("ChainData chainDataBytes json", string(chainDataBytes))

	assert.NoError(t, b.VerifyChainData(chainDataBytes))
}
