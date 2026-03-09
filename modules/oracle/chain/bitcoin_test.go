package chain

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestBitcoinRelayer(t *testing.T) {
	conf := NewChainConfig()
	assert.NoError(t, conf.Init())

	b := &bitcoinRelayer{conf: conf}
	assert.NoError(t, b.Init())

	chainState, err := b.GetLatestValidHeight()
	if err != nil {
		t.Fatal("failed GetLatestValidHeight", err)
	}

	t.Log("GetLatestValidHeight chainState", chainState)

	blocks, err := b.ChainData(chainState.blockHeight, 1)
	if err != nil {
		t.Fatal("failed ChainData", err)
	}
	t.Log("ChainData blocks", blocks)
}
