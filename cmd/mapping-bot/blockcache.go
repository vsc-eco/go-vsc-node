package main

// rawBlockCache keeps the last raw block the main loop downloaded, keyed by its
// hash. I-42 (testnet 11-05): while the next block waits to be deep enough in
// the contract (ConfirmationsRequired relayed blocks), the loop downloaded the
// same full block again on every pass: 472 times for block 5261810 in 100 min
// at the 10 s testnet interval; on mainnet (1 min, 4 confirmations, the public
// mempool.space API) about 30 downloads of each 1-2 MB block. The block's bytes
// are only read (wire.MsgBlock.Deserialize), and a reorg gives the height a new
// hash, so a hit is the same block.
type rawBlockCache struct {
	hash  string
	bytes []byte
}

// get returns hash's raw block, from the cache when it is the last one
// downloaded; a failed download is not cached.
func (c *rawBlockCache) get(hash string, fetch func(string) ([]byte, error)) ([]byte, error) {
	if hash != "" && hash == c.hash && c.bytes != nil {
		return c.bytes, nil
	}
	b, err := fetch(hash)
	if err != nil {
		return nil, err
	}
	c.hash, c.bytes = hash, b
	return b, nil
}
