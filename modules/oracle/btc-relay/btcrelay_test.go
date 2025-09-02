package btcrelay

import (
	"encoding/json"
	"testing"
	"vsc-node/modules/oracle/p2p"

	"github.com/stretchr/testify/assert"
)

func TestFetchChain(t *testing.T) {
	c := make(chan *p2p.BtcHeadBlock, 1)
	bcr := New(c)

	headBlock, err := bcr.fetchChain()
	assert.NoError(t, err)

	jsonBytes, _ := json.MarshalIndent(headBlock, "", "  ")
	t.Log(string(jsonBytes))
}
