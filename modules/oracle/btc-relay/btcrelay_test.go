package btcrelay

import (
	"encoding/json"
	"testing"
)

func TestFetchChain(t *testing.T) {
	bcr := New()

	c := make(chan *BtcHeadBlock, 1)
	bcr.fetchChain(c)

	headBlock := <-c
	jsonBytes, _ := json.MarshalIndent(headBlock, "", "  ")
	t.Log(string(jsonBytes))
}
