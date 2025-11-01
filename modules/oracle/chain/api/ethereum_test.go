package api

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestEthereumChainRelay(t *testing.T) {
	eth := &Ethereum{}

	if err := eth.Init(t.Context()); err != nil {
		t.Fatal(err)
	}

	t.Run("GetLatestValidHeight", testEthGetLatestValidHeight(eth))
}

func testEthGetLatestValidHeight(eth *Ethereum) func(t *testing.T) {
	return func(t *testing.T) {
		chainState, err := eth.GetLatestValidHeight()
		assert.NoError(t, err)
		t.Log("Latest block height", chainState.BlockHeight)
	}
}
