package btcrelay

import "testing"

func TestFetchChain(t *testing.T) {
	bcr := New()

	c := make(chan *BtcHeadBlock, 1)
	bcr.fetchChain(c)

	headBlock := <-c
	t.Log(*headBlock)
}
