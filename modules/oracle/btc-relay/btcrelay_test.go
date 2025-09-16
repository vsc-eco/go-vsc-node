package btcrelay

import (
	"encoding/json"
	"testing"
	"vsc-node/modules/oracle/p2p"

	"github.com/stretchr/testify/assert"
)

func TestFetchChain(t *testing.T) {
	c := make(chan *p2p.BlockRelay, 1)
	bcr := New(c)

	headBlock, err := bcr.FetchChain()
	assert.NoError(t, err)

	jsonBytes, _ := json.MarshalIndent(headBlock, "", "  ")
	t.Log(string(jsonBytes))
}

func TestFetchChainBtcd(t *testing.T) {
	// url := "http://127.0.0.1:8334"

}
