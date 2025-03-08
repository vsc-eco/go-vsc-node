package blockproducer

import (
	"encoding/base64"
	"math/big"

	"github.com/ethereum/go-ethereum/core/rawdb"
	trie "github.com/ethereum/go-ethereum/trie"
	"github.com/ethereum/go-ethereum/triedb"
	"github.com/ipfs/go-cid"
)

func MerklizeCids(txs []cid.Cid) (string, error) {

	merkleTree := trie.NewEmpty(triedb.NewDatabase(rawdb.NewMemoryDatabase(), nil))
	// proofDb := rawdb.NewMemoryDatabase()

	for index, cidz := range txs {
		bInt := big.NewInt(int64(index))
		err := merkleTree.Update(cidz.Bytes(), bInt.Bytes())
		if err != nil {
			return "", err
		}
	}

	hashBytes := merkleTree.Hash().Bytes()

	return base64.RawURLEncoding.EncodeToString(hashBytes), nil
}
