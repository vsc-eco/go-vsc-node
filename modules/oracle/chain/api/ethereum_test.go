package api

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
)

type ethTestRunner struct{ *Ethereum }

func TestEthereumChainRelay(t *testing.T) {
	eth := ethTestRunner{&Ethereum{}}

	if err := eth.Init(t.Context()); err != nil {
		t.Fatal(err)
	}

	// t.Run("GetLatestValidHeight", eth.testGetLatestValidHeight)
	t.Run("ChainData", eth.testChainData)
}

func (e *ethTestRunner) testChainData(t *testing.T) {
	const blockCtr = 10
	chainData, err := e.ChainData(20728016, blockCtr)
	assert.NoError(t, err)
	assert.Equal(t, blockCtr, len(chainData))

	for _, block := range chainData {
		blockHeight, err := block.BlockHeight()
		if err != nil {
			t.Fatal(err)
		}

		avgGas, err := block.AverageFee()
		if err != nil {
			t.Fatal(err)
		}

		b := map[string]any{
			"block height": blockHeight,
			"avgerage fee": fmt.Sprintf("%f", float64(avgGas)/1e18),
		}

		bb, _ := json.MarshalIndent(b, "", "  ")

		t.Log(string(bb))
	}
}

func (e *ethTestRunner) testGetLatestValidHeight(t *testing.T) {
	chainState, err := e.GetLatestValidHeight()
	assert.NoError(t, err)
	t.Log("Latest block height", chainState.BlockHeight)
}
